package vcore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/veypi/vigo/contrib/ufs"
)

// 一致性测试向量运行器（§2.6）：输入 = argv/params + 初始文件树，
// 期望 = Content/Attrs/错误消息/最终文件树。Go（本测试）与 JS（spike-js）
// 跑同一套向量，语义分歧在 CI 暴露。

type vectorFile struct {
	Cases []vectorCase `json:"cases"`
}

type vectorCase struct {
	Name         string            `json:"name"`
	Cmd          string            `json:"cmd"` // ls/find/grep/...；fs 三操作填 "fs"
	Argv         []string          `json:"argv"`
	Params       json.RawMessage   `json:"params"` // fs 原生 JSON 参数
	Workdir      string            `json:"workdir"`
	Vars         map[string]string `json:"vars"`
	ProtectRoots []string          `json:"protectRoots"`
	Files        map[string]string `json:"files"`  // "/a.txt" → 内容；"/dir/" 目录
	Mtimes       map[string]int64  `json:"mtimes"` // 路径 → unix 秒
	Fetch        map[string]string `json:"fetch"`  // curl：url → body
	Expect       *vectorExpect     `json:"expect"`
	ExpectError  string            `json:"expectError"`
	ExpectFiles  map[string]string `json:"expectFiles"` // 执行后的最终文件树（可选）
}

type vectorExpect struct {
	Content string            `json:"content"`
	Attrs   map[string]string `json:"attrs"`
}

func TestVectors(t *testing.T) {
	files, err := filepath.Glob("testdata/vectors/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("no vectors found: %v", err)
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var vf vectorFile
		if err := json.Unmarshal(data, &vf); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		for _, c := range vf.Cases {
			t.Run(filepath.Base(f)+"/"+c.Name, func(t *testing.T) {
				runVectorCase(t, c)
			})
		}
	}
}

func runVectorCase(t *testing.T, c vectorCase) {
	t.Helper()
	vfs := NewMemVFS()
	for p, content := range c.Files {
		mt := time.Unix(c.Mtimes[p], 0)
		if strings.HasSuffix(p, "/") {
			vfs.SetDir(strings.TrimSuffix(p, "/"), mt)
		} else {
			vfs.SetFile(p, vectorBytes(t, content), mt)
		}
	}
	// mtimes 可独立作用于 files 未列出的隐式目录（如 "/docs/sub/"）
	for p, sec := range c.Mtimes {
		if _, listed := c.Files[p]; !listed && strings.HasSuffix(p, "/") {
			vfs.SetDir(strings.TrimSuffix(p, "/"), time.Unix(sec, 0))
		}
	}
	env := &Env{
		VFS:          vfs,
		Workdir:      c.Workdir,
		Vars:         c.Vars,
		ProtectRoots: c.ProtectRoots,
	}
	if c.Fetch != nil {
		env.Fetcher = FetchFunc(func(ctx context.Context, rawurl string) (io.ReadCloser, int64, error) {
			body, ok := c.Fetch[rawurl]
			if !ok {
				return nil, 0, fmt.Errorf("404 not found")
			}
			data := vectorBytes(t, body)
			return io.NopCloser(strings.NewReader(string(data))), int64(len(data)), nil
		})
	}
	// 任务托管（curl 无 -o）：同步执行的内存实现
	env.Tasks = testTaskRunner{}
	env.TaskID = "vec"

	var res *Result
	var err error
	if c.Cmd == "fs" {
		res, err = RunFS(context.Background(), env, c.Params)
	} else {
		res, err = Run(context.Background(), env, c.Cmd, c.Argv)
	}

	if c.ExpectError != "" {
		if err == nil {
			t.Fatalf("want error %q, got result %+v", c.ExpectError, res)
		}
		if err.Error() != c.ExpectError {
			t.Fatalf("error = %q, want %q", err.Error(), c.ExpectError)
		}
	} else if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if c.Expect != nil {
		if res.Content != c.Expect.Content {
			t.Errorf("content = %q, want %q", res.Content, c.Expect.Content)
		}
		if !reflect.DeepEqual(res.Attrs, c.Expect.Attrs) {
			t.Errorf("attrs = %v, want %v", res.Attrs, c.Expect.Attrs)
		}
	}

	if c.ExpectFiles != nil {
		got := dumpTree(t, vfs)
		if !reflect.DeepEqual(got, c.ExpectFiles) {
			t.Errorf("final tree = %v, want %v", got, c.ExpectFiles)
		}
	}
}

// testTaskRunner 是向量测试的同步 TaskRunner：任务体输出直接作为结果返回。
type testTaskRunner struct{}

func (testTaskRunner) StartTask(ctx context.Context, opts TaskOptions) (*TaskResult, error) {
	var buf strings.Builder
	if err := opts.Run(ctx, &buf); err != nil {
		return nil, err
	}
	return &TaskResult{
		Content: buf.String(),
		Lines:   strings.Count(buf.String(), "\n"),
		ID:      opts.ID,
		LogPath: "/.exec/" + opts.ID + ".log",
	}, nil
}

// vectorBytes 解码向量内容指令：
//   - "base64:..."  → 二进制内容（无效 UTF-8 等 JSON 表达不了的字节）
//   - "repeat:{n}MB" → n MB 的 'x'（大文件/超限场景）
//   - 其余原样
func vectorBytes(t *testing.T, s string) []byte {
	t.Helper()
	if b64, ok := strings.CutPrefix(s, "base64:"); ok {
		data, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			t.Fatalf("bad base64 in vector: %v", err)
		}
		return data
	}
	if spec, ok := strings.CutPrefix(s, "repeat:"); ok {
		var n int
		if _, err := fmt.Sscanf(spec, "%dMB", &n); err != nil {
			t.Fatalf("bad repeat spec %q: %v", spec, err)
		}
		return make([]byte, n<<20)
	}
	return []byte(s)
}

// dumpTree 导出文件树：目录为 "path/"，文件为 path → 内容。
func dumpTree(t *testing.T, vfs ufs.FS) map[string]string {
	t.Helper()
	out := map[string]string{}
	var walk func(dir string)
	walk = func(dir string) {
		entries, err := vfs.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			full := path.Join(dir, e.Name())
			if e.IsDir() {
				out[full+"/"] = ""
				walk(full)
			} else {
				data, _ := vfs.ReadFile(full)
				out[full] = string(data)
			}
		}
	}
	walk("/")
	return out
}
