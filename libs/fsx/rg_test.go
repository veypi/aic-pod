package fsx

// rg_test.go — rg 的 limit/context 越界钳制与失败归类（2026-10-10）。
// 语义：越界不报错，按上限执行，实际生效值回显在 attrs（仅钳制时出现）；
// 参数类失败自报 invalid_argument，不再落 protocol.AsFault 的 internal 兜底。

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/veypi/aic-pod/protocol"
	"github.com/veypi/vigo/contrib/ufs"
)

func TestRgClampsContextAndLimit(t *testing.T) {
	env := newTestEnv(t)
	runWrite(t, env, "/a.txt", "l1\nl2\nmatch\nl4\nl5\nl6\n")

	// 越界：context 14 → 10，limit 9999 → 200，均按上限执行并回显生效值
	r := runFS(t, env, map[string]any{
		"action": "rg", "path": "/a.txt", "pattern": "match",
		"context": 14, "limit": 9999,
	})
	if r.Attrs["context"] != "10" {
		t.Fatalf("context 未钳制到上限: %v", r.Attrs)
	}
	if r.Attrs["limit"] != "200" {
		t.Fatalf("limit 未钳制到上限: %v", r.Attrs)
	}
	if _, ok := r.Attrs["rows"]; ok {
		t.Fatalf("rows 应从正文统计，不应复制到 attrs: %v", r.Attrs)
	}
	if _, ok := r.Attrs["action"]; ok {
		t.Fatalf("action 已由请求表达，不应复制到 attrs: %v", r.Attrs)
	}

	// 负值同样钳制到下限（不报错）
	r = runFS(t, env, map[string]any{
		"action": "rg", "path": "/a.txt", "pattern": "match", "context": -3,
	})
	if r.Attrs["context"] != "0" {
		t.Fatalf("context 未钳制到下限: %v", r.Attrs)
	}

	// 未越界：不回显（参数原样生效，attrs 无噪音）
	r = runFS(t, env, map[string]any{
		"action": "rg", "path": "/a.txt", "pattern": "match", "context": 2,
	})
	if _, ok := r.Attrs["context"]; ok {
		t.Fatalf("未越界不该回显 context: %v", r.Attrs)
	}
	if _, ok := r.Attrs["limit"]; ok {
		t.Fatalf("未指定 limit 不该回显: %v", r.Attrs)
	}

	// 列举模式（pattern 缺省）同样吃 limit 钳制
	r = runFS(t, env, map[string]any{"action": "rg", "path": "/", "limit": 5000})
	if r.Attrs["limit"] != "200" {
		t.Fatalf("列举模式 limit 未钳制: %v", r.Attrs)
	}
}

type readDirCountFS struct {
	ufs.FS
	reads map[string]int
}

func (f *readDirCountFS) ReadDir(name string) ([]fs.DirEntry, error) {
	f.reads[name]++
	return f.FS.ReadDir(name)
}

func TestRgStopsWalkingAtLimit(t *testing.T) {
	t.Run("content", func(t *testing.T) {
		env := newTestEnv(t)
		runWrite(t, env, "/a/hit.txt", "match\n")
		runWrite(t, env, "/z/unreached.txt", "match\n")
		counter := &readDirCountFS{FS: env.FS, reads: map[string]int{}}
		env.FS = counter

		r := runFS(t, env, map[string]any{
			"action": "rg", "path": "/", "pattern": "match", "limit": 1,
		})
		if r.Attrs["truncated"] != "true" {
			t.Fatalf("limit 结果不正确: %v", r.Attrs)
		}
		if counter.reads["/z"] != 0 {
			t.Fatal("达到 limit 后仍遍历了后续目录")
		}
	})

	t.Run("file list", func(t *testing.T) {
		env := newTestEnv(t)
		runWrite(t, env, "/a/first.txt", "first\n")
		runWrite(t, env, "/z/unreached.txt", "second\n")
		counter := &readDirCountFS{FS: env.FS, reads: map[string]int{}}
		env.FS = counter

		r := runFS(t, env, map[string]any{
			"action": "rg", "path": "/", "limit": 1,
		})
		if r.Attrs["truncated"] != "true" {
			t.Fatalf("limit 结果不正确: %v", r.Attrs)
		}
		if counter.reads["/z"] != 0 {
			t.Fatal("文件列举达到 limit 后仍遍历了后续目录")
		}
	})
}

type readDirErrorFS struct {
	ufs.FS
	path string
	err  error
}

func (f *readDirErrorFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if name == f.path {
		return nil, f.err
	}
	return f.FS.ReadDir(name)
}

func TestRgSkipsPermissionDeniedDirectories(t *testing.T) {
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{name: "content", args: map[string]any{"action": "rg", "path": "/", "pattern": "match"}},
		{name: "file list", args: map[string]any{"action": "rg", "path": "/"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			runWrite(t, env, "/a/denied/secret.txt", "match\n")
			runWrite(t, env, "/a/visible.txt", "match\n")
			env.FS = &readDirErrorFS{FS: env.FS, path: "/a/denied", err: fs.ErrPermission}

			r := runFS(t, env, tc.args)
			if !strings.Contains(r.Content, "visible.txt") {
				t.Fatalf("可读兄弟目录未被搜索: %s", r.Content)
			}
			if strings.Contains(r.Content, "secret.txt") {
				t.Fatalf("权限拒绝目录不应出现在结果中: %s", r.Content)
			}
		})
	}
}

func TestRgSingleFileAtLimitIsNotTruncated(t *testing.T) {
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"content", map[string]any{"action": "rg", "path": "/only.txt", "pattern": "match", "limit": 1}},
		{"file list", map[string]any{"action": "rg", "path": "/only.txt", "limit": 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			runWrite(t, env, "/only.txt", "match\n")
			r := runFS(t, env, tc.args)
			if _, ok := r.Attrs["truncated"]; ok {
				t.Fatalf("单文件恰好达到 limit 不该输出 truncated: %v", r.Attrs)
			}
		})
	}
}

func TestFsErrClassifiedAsInvalidArgument(t *testing.T) {
	// 参数/语义类失败：Error() 文案不变（§2.3 锁定），Fault 码为 invalid_argument
	err := fsErr("rg", "context is only valid for content search (pattern is required)")
	if got, want := err.Error(), "fs rg: context is only valid for content search (pattern is required)"; got != want {
		t.Fatalf("文案被改动: %q", got)
	}
	if f := protocol.AsFault(err); f.Code != "invalid_argument" {
		t.Fatalf("未归类为 invalid_argument: %+v", f)
	}
	// 能力类拒绝走 unsupported
	if f := protocol.AsFault(fsUnsupportedErr("mv", "gone")); f.Code != "unsupported" {
		t.Fatalf("未归类为 unsupported: %+v", f)
	}
	// fsOpErr（OS 层错误）保持既有链：ENOENT 仍是 not_found
	if f := protocol.AsFault(err); f.Message != err.Error() {
		t.Fatalf("Message 不该被改写: %+v", f)
	}
}
