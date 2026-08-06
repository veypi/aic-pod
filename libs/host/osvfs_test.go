package host

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/veypi/aic-pod/libs/vcore"
)

// 阶段 8 门禁：vcore 一致性测试向量在 OS VFS 适配下通过（§2.6 零漂移）。
// 策略与 aic 侧 UFS 门禁相同：同一用例在 OSVFS 与 MemVFS 上分别播种、执行，
// Content/Attrs/错误必须一致（目录 size 环境差异归一）。
type osVectorCase struct {
	Name    string            `json:"name"`
	Cmd     string            `json:"cmd"`
	Argv    []string          `json:"argv"`
	Params  json.RawMessage   `json:"params"`
	Workdir string            `json:"workdir"`
	Vars    map[string]string `json:"vars"`
	Files        map[string]string `json:"files"`
	Mtimes       map[string]int64  `json:"mtimes"`
	Fetch        map[string]string `json:"fetch"`
	ProtectRoots []string          `json:"protectRoots"`
}

func TestVcoreVectorsOnOSVFS(t *testing.T) {
	vecDir := filepath.Join("..", "vcore", "testdata", "vectors")
	files, err := filepath.Glob(filepath.Join(vecDir, "*.json"))
	if err != nil || len(files) == 0 {
		t.Skipf("vcore vectors not found at %s", vecDir)
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var vf struct {
			Cases []osVectorCase `json:"cases"`
		}
		if err := json.Unmarshal(data, &vf); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		for _, c := range vf.Cases {
			t.Run(filepath.Base(f)+"/"+c.Name, func(t *testing.T) {
				runOSParity(t, c)
			})
		}
	}
}

var osDirSizeRe = regexp.MustCompile(`(/\t)\d+(\t)`)

// treeJSONRe 归一 tree JSON 的 size/mod_time（环境数据差异，§2.6；文件大小由 ls -la 覆盖）。
var treeJSONRe = regexp.MustCompile(`"(size|mod_time)":\d+`)

func normContent(s string) string {
	s = osDirSizeRe.ReplaceAllString(s, "${1}D${2}")
	return treeJSONRe.ReplaceAllString(s, `"${1}":0`)
}

func runOSParity(t *testing.T, c osVectorCase) {
	t.Helper()
	mem := vcore.NewMemVFS()
	for p, content := range c.Files {
		mt := time.Unix(c.Mtimes[p], 0)
		if strings.HasSuffix(p, "/") {
			mem.SetDir(strings.TrimSuffix(p, "/"), mt)
		} else {
			mem.SetFile(p, osVecBytes(t, content), mt)
		}
	}

	root := t.TempDir()
	osv := &subVFS{root: root}
	for p, content := range c.Files {
		logical := strings.TrimSuffix(p, "/")
		if strings.HasSuffix(p, "/") {
			if err := osv.MkdirAll(logical); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := osv.MkdirAll(pathDirOf(logical)); err != nil {
				t.Fatal(err)
			}
			if err := osv.WriteFile(logical, osVecBytes(t, content)); err != nil {
				t.Fatal(err)
			}
		}
		if sec, ok := c.Mtimes[p]; ok {
			_ = os.Chtimes(osv.mapPath(logical), time.Unix(sec, 0), time.Unix(sec, 0))
		}
	}
	// mtimes 可独立作用于 files 未列出的隐式目录（与 vcore 向量运行器一致）
	for p, sec := range c.Mtimes {
		if _, listed := c.Files[p]; listed || !strings.HasSuffix(p, "/") {
			continue
		}
		logical := strings.TrimSuffix(p, "/")
		mt := time.Unix(sec, 0)
		mem.SetDir(logical, mt)
		if err := osv.MkdirAll(logical); err != nil {
			t.Fatal(err)
		}
		_ = os.Chtimes(osv.mapPath(logical), mt, mt)
	}

	fetcher := vcore.Fetcher(nil)
	if c.Fetch != nil {
		fetcher = vcore.FetchFunc(func(ctx context.Context, rawurl string) (io.ReadCloser, int64, error) {
			body, ok := c.Fetch[rawurl]
			if !ok {
				return nil, 0, fmt.Errorf("404 not found")
			}
			data := osVecBytes(t, body)
			return io.NopCloser(strings.NewReader(string(data))), int64(len(data)), nil
		})
	}
	// OSVFS 测试 root 隔离：逻辑绝对路径直接映射到 OS 绝对路径会污染真实文件系统——
	// 用 chroot 式子树适配（测试专用）：将逻辑路径重写到 t.TempDir() 下。
	osEnv := &vcore.Env{VFS: osv, Workdir: c.Workdir, Vars: c.Vars, Fetcher: fetcher, ProtectRoots: c.ProtectRoots}
	memEnv := &vcore.Env{VFS: mem, Workdir: c.Workdir, Vars: c.Vars, Fetcher: fetcher, ProtectRoots: c.ProtectRoots}

	var memRes, osRes *vcore.Result
	var memErr, osErr error
	if c.Cmd == "fs" {
		memRes, memErr = vcore.RunFS(context.Background(), memEnv, c.Params)
		osRes, osErr = vcore.RunFS(context.Background(), osEnv, c.Params)
	} else {
		memRes, memErr = vcore.Run(context.Background(), memEnv, c.Cmd, c.Argv)
		osRes, osErr = vcore.Run(context.Background(), osEnv, c.Cmd, c.Argv)
	}

	if (memErr == nil) != (osErr == nil) {
		t.Fatalf("error mismatch: mem=%v os=%v", memErr, osErr)
	}
	if memErr != nil && memErr.Error() != osErr.Error() {
		t.Fatalf("error text mismatch:\nmem=%q\nos=%q", memErr, osErr)
	}
	if memRes != nil && osRes != nil {
		if normContent(memRes.Content) != normContent(osRes.Content) {
			t.Errorf("content mismatch:\nmem=%q\nos=%q", memRes.Content, osRes.Content)
		}
		if !reflect.DeepEqual(memRes.Attrs, osRes.Attrs) {
			t.Errorf("attrs mismatch:\nmem=%v\nos=%v", memRes.Attrs, osRes.Attrs)
		}
	}
}

// subVFS 将逻辑绝对路径重写到子树下的测试适配（避免污染真实文件系统）。
type subVFS struct{ root string }

func (s *subVFS) mapPath(p string) string {
	p = strings.TrimPrefix(p, "/")
	return filepath.ToSlash(filepath.Join(s.root, filepath.FromSlash(p)))
}

var osvDelegate = OSVFS{}

func (s *subVFS) Stat(name string) (fs.FileInfo, error) { return osvDelegate.Stat(s.mapPath(name)) }
func (s *subVFS) ReadDir(name string) ([]fs.DirEntry, error) {
	return osvDelegate.ReadDir(s.mapPath(name))
}
func (s *subVFS) ReadFile(name string) ([]byte, error) { return osvDelegate.ReadFile(s.mapPath(name)) }
func (s *subVFS) Open(name string) (fs.File, error)    { return osvDelegate.Open(s.mapPath(name)) }
func (s *subVFS) Create(name string) (io.WriteCloser, error) {
	return osvDelegate.Create(s.mapPath(name))
}
func (s *subVFS) WriteFile(name string, data []byte) error {
	return osvDelegate.WriteFile(s.mapPath(name), data)
}
func (s *subVFS) MkdirAll(name string) error  { return osvDelegate.MkdirAll(s.mapPath(name)) }
func (s *subVFS) RemoveAll(name string) error { return osvDelegate.RemoveAll(s.mapPath(name)) }
func (s *subVFS) Rename(oldname, newname string) error {
	return osvDelegate.Rename(s.mapPath(oldname), s.mapPath(newname))
}

func pathDirOf(p string) string {
	if i := strings.LastIndex(p, "/"); i > 0 {
		return p[:i]
	}
	return "/"
}

func osVecBytes(t *testing.T, s string) []byte {
	t.Helper()
	if b64, ok := strings.CutPrefix(s, "base64:"); ok {
		data, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	if spec, ok := strings.CutPrefix(s, "repeat:"); ok {
		var n int
		if _, err := fmt.Sscanf(spec, "%dMB", &n); err != nil {
			t.Fatal(err)
		}
		return make([]byte, n<<20)
	}
	return []byte(s)
}
