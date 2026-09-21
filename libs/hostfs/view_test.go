//go:build darwin || linux || windows

package hostfs

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/veypi/aic-pod/libs/vcore"
	hosts "github.com/veypi/aic-pod/protocol/fs"
)

// Exercise the AI adapter as well as the binary FS methods: the Windows
// regression only became visible when AI tools switched from OSVFS to View.
func TestAIFileOperationsUseNativeView(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	root := f.fs.roots["home"].Path
	path := func(name string) string { return filepath.ToSlash(filepath.Join(root, name)) }
	env := func() *vcore.Env {
		return &vcore.Env{VFS: f.fs.View(ctx, f.caller), Workdir: filepath.ToSlash(root), Granted: 9}
	}
	run := func(args map[string]any) *vcore.Result {
		t.Helper()
		raw, _ := json.Marshal(args)
		result, err := vcore.RunFS(ctx, env(), raw)
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
	run(map[string]any{"action": "cp", "src": path("note.txt"), "dst": path("copy.txt")})
	run(map[string]any{"action": "mv", "src": path("copy.txt"), "dst": path("moved.txt")})
	if data, err := os.ReadFile(path("moved.txt")); err != nil || string(data) != "second\n" {
		t.Fatalf("copy/move: %q %v", data, err)
	}
	if _, err := os.Stat(path("copy.txt")); !os.IsNotExist(err) {
		t.Fatal("move left its source", err)
	}
	download := env()
	download.Fetcher = vcore.FetchFunc(func(context.Context, vcore.HTTPReq) (io.ReadCloser, int64, error) {
		return io.NopCloser(strings.NewReader("download")), 8, nil
	})
	if _, err := vcore.Run(ctx, download, "curl", []string{"-o", path("download.bin"), "https://example.test/file"}); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path("download.bin")); err != nil || string(data) != "download" {
		t.Fatalf("curl output: %q %v", data, err)
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
	if err := view.WriteFile(path, []byte("stale"), 0600); err == nil || hosts.AsFault(err).Code != "version_conflict" {
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
					return hosts.Fail("permission_denied", "only the target file is allowed")
				}
				return nil
			}
			ctx := context.Background()
			env := &vcore.Env{VFS: f.fs.View(ctx, f.caller), Workdir: filepath.ToSlash(root), Granted: 9}
			raw, _ := json.Marshal(map[string]any{"action": "write", "path": filepath.ToSlash(file), "content": "granted file"})
			_, err := vcore.RunFS(ctx, env, raw)
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
