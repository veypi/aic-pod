//go:build darwin || linux || windows

package hostfs

import (
	"context"
	"encoding/json"
	"github.com/veypi/aic-pod/protocol"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/veypi/aic-pod/libs/fsx"
)

// Exercise the AI adapter as well as the binary FS methods: the Windows
// regression only became visible when AI tools switched from OSVFS to View.
// （cp/mv/rm 随 v4.1 下线由引擎内建承接；curl 由引擎 NetClient 承接——
// 两者均不在 fsx 五 action 面。）
func TestAIFileOperationsUseNativeView(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	root := f.fs.roots["home"].Path
	path := func(name string) string { return filepath.ToSlash(filepath.Join(root, name)) }
	env := func() *fsx.Env {
		return &fsx.Env{FS: f.fs.View(ctx, f.caller), Workdir: filepath.ToSlash(root)}
	}
	run := func(args map[string]any) *fsx.Result {
		t.Helper()
		raw, _ := json.Marshal(args)
		result, err := fsx.RunFS(ctx, env(), raw)
		if err != nil {
			t.Fatalf("%s: %v", args["action"], err)
		}
		return result
	}
	run(map[string]any{"action": "write", "path": path("note.txt"), "content": "first\n"})
	run(map[string]any{"action": "edit", "path": path("note.txt"), "edits": []map[string]string{{"oldText": "first", "newText": "second"}}})
	for _, args := range []map[string]any{
		{"action": "read", "path": path("note.txt")},
		{"action": "rg", "path": path("note.txt"), "pattern": "second"},
	} {
		data, _ := json.Marshal(run(args))
		if !strings.Contains(string(data), "second") {
			t.Fatalf("missing file content: %s", data)
		}
	}
	// 下线 action 报可读错误（引导 exec）。
	raw, _ := json.Marshal(map[string]any{"action": "cp", "src": path("note.txt"), "dst": path("copy.txt")})
	if _, err := fsx.RunFS(ctx, env(), raw); err == nil || !strings.Contains(err.Error(), "exec") {
		t.Fatalf("cp 应报已下线引导 exec: %v", err)
	}
}

// TestViewErrorsEchoCallerPath fs 报错回显调用者给的完整路径：pod 侧 os.Root 系调用
// 只报根内相对名（"statat fsedit-nope: no such file or directory"），平台 fs 工具把
// 它原样给 AI，AI 无法据此纠偏（2026-10-07 实测走设备 fs read 复现）。
func TestViewErrorsEchoCallerPath(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	root := f.fs.roots["home"].Path
	env := &fsx.Env{FS: f.fs.View(ctx, f.caller), Workdir: filepath.ToSlash(root)}
	missing := filepath.ToSlash(filepath.Join(root, "no-such-dir", "no-such.txt"))
	for _, action := range []string{"read", "ls", "edit"} {
		args := map[string]any{"action": action, "path": missing}
		if action == "edit" {
			args["edits"] = []map[string]string{{"oldText": "x", "newText": "y"}}
		}
		raw, _ := json.Marshal(args)
		_, err := fsx.RunFS(ctx, env, raw)
		if err == nil {
			t.Fatalf("%s(%s) err = nil; want not-exist error", action, missing)
		}
		if !strings.Contains(err.Error(), missing) {
			t.Fatalf("%s 报错未回显调用者完整路径 %s：%v", action, missing, err)
		}
	}
}

func TestViewRejectsStaleWrite(t *testing.T) {
	f := setup(t)
	path := filepath.ToSlash(filepath.Join(f.fs.roots["home"].Path, "note.txt"))
	if err := os.WriteFile(path, []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	view := f.fs.View(context.Background(), f.caller)
	if _, err := view.ReadFile(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("changed externally"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := view.WriteFile(path, []byte("stale"), 0600); err == nil || protocol.AsFault(err).Code != "version_conflict" {
		t.Fatal("stale write accepted", err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "changed externally" {
		t.Fatal("stale write changed destination", err)
	}
}

// A grant for one file must be sufficient for the AI write path even when its
// existing parent is not writable. Missing parents still need their own grant.
func TestAIWriteWithOnlyTargetFileAllowed(t *testing.T) {
	for _, parentsExist := range []bool{true, false} {
		t.Run(map[bool]string{true: "existing_parent", false: "missing_parent"}[parentsExist], func(t *testing.T) {
			f := setup(t)
			root := f.fs.roots["home"].Path
			parent := filepath.Join(root, "container")
			file := filepath.Join(parent, "allowed.txt")
			if parentsExist {
				if err := os.Mkdir(parent, 0700); err != nil {
					t.Fatal(err)
				}
			}
			f.fs.cfg.Check = func(_ context.Context, _ Call, path string, write bool) error {
				if filepath.Clean(path) != file {
					return protocol.FSFail("permission_denied", "only the target file is allowed")
				}
				return nil
			}
			ctx := context.Background()
			env := &fsx.Env{FS: f.fs.View(ctx, f.caller), Workdir: filepath.ToSlash(root)}
			raw, _ := json.Marshal(map[string]any{"action": "write", "path": filepath.ToSlash(file), "content": "granted file"})
			_, err := fsx.RunFS(ctx, env, raw)
			if parentsExist {
				if err != nil {
					t.Fatal("file-only grant failed", err)
				}
				if data, err := os.ReadFile(file); err != nil || string(data) != "granted file" {
					t.Fatalf("file content: %q, %v", data, err)
				}
			} else {
				if err == nil {
					t.Fatal("created an ungranted parent")
				}
				if _, err := os.Stat(parent); !os.IsNotExist(err) {
					t.Fatal("denied write had a filesystem effect", err)
				}
			}
		})
	}
}
