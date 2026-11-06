package fsx

import (
	"errors"
	"io/fs"
	"testing"
)

// TestFSOpErrMatchesErrText fsOpErr 的文案必须与旧写法 fsErr(action, "%s", err)
// 逐字一致（调用方与 AI 看到的消息不变），差别只在错误链是否可达。
func TestFSOpErrMatchesErrText(t *testing.T) {
	inner := fs.ErrNotExist
	if got, want := fsOpErr("read", inner).Error(), fsErr("read", "%s", inner).Error(); got != want {
		t.Fatalf("fsOpErr 文案 = %q; want %q", got, want)
	}
	if got, want := fsOpErr("", inner).Error(), fsErr("", "%s", inner).Error(); got != want {
		t.Fatalf("空 action 文案 = %q; want %q", got, want)
	}
	if !errors.Is(fsOpErr("read", inner), fs.ErrNotExist) {
		t.Fatal("fsOpErr 丢了错误链（errors.Is 不可达）")
	}
	if fsOpErr("read", nil) != nil {
		t.Fatal("fsOpErr(nil) 应返回 nil")
	}
}

func TestReadAttrsMinimal(t *testing.T) {
	env := newTestEnv(t)
	runWrite(t, env, "/r.txt", "a\nb\nc\n")
	r := runFS(t, env, map[string]any{"action": "read", "path": "/r.txt", "offset": 2, "limit": 1})
	if r.Content != "2\tb\n" {
		t.Fatalf("read 正文 = %q", r.Content)
	}
	if len(r.Attrs) != 3 || r.Attrs["path"] != "/r.txt" || r.Attrs["range"] != "2-2" || r.Attrs["total_lines"] != "3" {
		t.Fatalf("read attrs 只应保留 path/range/total_lines: %v", r.Attrs)
	}
}
