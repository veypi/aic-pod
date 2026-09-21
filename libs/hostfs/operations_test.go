//go:build darwin || linux

package hostfs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	fsp "github.com/veypi/aic-pod/protocol/fs"
	hosts "github.com/veypi/aic-pod/protocol/fs"
)

func TestCancelledCopyRetainsExplicitPartialEffects(t *testing.T) {
	f := setup(t)
	value[fsp.Entry](t, f.call(t, "mkdir", mkdirArgs{Path: loc("src")}))
	for _, name := range []string{"a", "b"} {
		value[fsp.Entry](t, f.call(t, "write", writeArgs{Path: loc("src", name), Source: f.upload(t, []byte(name)), Condition: fsp.Condition{Absent: true}}))
	}
	src := value[fsp.Entry](t, f.call(t, "stat", pathArgs{Path: loc("src")}))
	check := f.fs.cfg.Check
	f.fs.cfg.Check = func(ctx context.Context, call Call, path string, write bool) error {
		if filepath.Base(path) == "b" {
			if _, err := os.Stat(filepath.Join(f.root, "dst", "a")); err == nil {
				return context.Canceled
			}
		}
		return check(ctx, call, path, write)
	}
	op := f.call(t, "copy", moveArgs{Src: src.Path, Dst: loc("dst"), IfVersion: src.Version, Condition: fsp.Condition{Absent: true}})
	if op.Status != "cancelled" || op.Error == nil || op.Error.Effect != "partial" || op.Error.Details.(map[string]any)["completed"] != 2 {
		t.Fatalf("copy cancellation hid committed entries: %+v / %+v", op, op.Error)
	}
	if data, err := os.ReadFile(filepath.Join(f.root, "dst", "a")); err != nil || string(data) != "a" {
		t.Fatal("completed copy disappeared")
	}
	if _, err := os.Stat(filepath.Join(f.root, "dst", "b")); !os.IsNotExist(err) {
		t.Fatal("cancelled copy committed the next file")
	}
}

func TestTreeCopyMoveFindAndRemove(t *testing.T) {
	f := setup(t)
	value[fsp.Entry](t, f.call(t, "mkdir", mkdirArgs{Path: loc("src", "nested"), Parents: true}))
	value[fsp.Entry](t, f.call(t, "write", writeArgs{Path: loc("src", "nested", "中文%.txt"), Source: f.upload(t, []byte("\xef\xbb\xbfx\r\n")), Condition: fsp.Condition{Absent: true}}))
	src := value[fsp.Entry](t, f.call(t, "stat", pathArgs{Path: loc("src")}))
	value[map[string]any](t, f.call(t, "copy", moveArgs{Src: src.Path, Dst: loc("clone"), IfVersion: src.Version, Condition: fsp.Condition{Absent: true}}))
	body, err := os.ReadFile(filepath.Join(f.root, "clone", "nested", "中文%.txt"))
	if err != nil || string(body) != "\xef\xbb\xbfx\r\n" {
		t.Fatalf("copy corrupted content: %q %v", body, err)
	}
	clone := value[fsp.Entry](t, f.call(t, "stat", pathArgs{Path: loc("clone")}))
	conflict := f.call(t, "move", moveArgs{Src: clone.Path, Dst: loc("src"), IfVersion: clone.Version, Condition: fsp.Condition{Absent: true}})
	if conflict.Error == nil || conflict.Error.Code != "already_exists" {
		t.Fatalf("move replaced an existing directory: %+v", conflict)
	}
	moved := value[fsp.Entry](t, f.call(t, "move", moveArgs{Src: clone.Path, Dst: loc("moved"), IfVersion: clone.Version, Condition: fsp.Condition{Absent: true}}))
	found := value[struct {
		Entries   []fsp.Entry `json:"entries"`
		Truncated bool        `json:"truncated"`
	}](t, f.call(t, "find", findArgs{Path: loc(), Glob: "*.txt", Depth: 5, Limit: 10}))
	if len(found.Entries) != 2 || found.Truncated {
		t.Fatalf("find: %+v", found)
	}
	value[map[string]any](t, f.call(t, "remove", removeArgs{Path: moved.Path, IfVersion: moved.Version, Recursive: true}))
	if _, err = os.Stat(filepath.Join(f.root, "moved")); !os.IsNotExist(err) {
		t.Fatalf("tree still present: %v", err)
	}
	if _, err = os.Stat(filepath.Join(f.root, "src")); err != nil {
		t.Fatal("removed the source tree", err)
	}
}

func TestTreeOperationsValidateEntirePlanBeforeEffects(t *testing.T) {
	for _, method := range []string{"copy", "move", "remove"} {
		t.Run(method, func(t *testing.T) {
			f := setup(t)
			if err := os.MkdirAll(filepath.Join(f.root, "src"), 0700); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"allowed", "denied"} {
				if err := os.WriteFile(filepath.Join(f.root, "src", name), []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			src := value[fsp.Entry](t, f.call(t, "stat", pathArgs{Path: loc("src")}))
			var op outcome
			if method == "remove" {
				op = f.call(t, method, removeArgs{Path: src.Path, IfVersion: src.Version, Recursive: true})
			} else {
				op = f.call(t, method, moveArgs{Src: src.Path, Dst: loc("dst"), IfVersion: src.Version, Condition: fsp.Condition{Absent: true}})
			}
			if op.Error == nil || op.Error.Code != "permission_denied" || op.Error.Effect != "none" {
				t.Fatalf("policy failure: %+v", op)
			}
			if _, err := os.Stat(filepath.Join(f.root, "src", "allowed")); err != nil {
				t.Fatal("partial mutation before plan validation", err)
			}
			if _, err := os.Stat(filepath.Join(f.root, "dst")); !os.IsNotExist(err) {
				t.Fatal("created target on denial")
			}
		})
	}
}

func TestRecursiveCopyRejectsSelfAndMkdirPreflightsDeniedPaths(t *testing.T) {
	f := setup(t)
	src := value[fsp.Entry](t, f.call(t, "mkdir", mkdirArgs{Path: loc("src")}))
	op := f.call(t, "copy", moveArgs{Src: src.Path, Dst: loc("src", "inside"), IfVersion: src.Version, Condition: fsp.Condition{Absent: true}})
	if op.Error == nil || op.Error.Code != "invalid_argument" {
		t.Fatalf("recursive self copy accepted: %+v", op)
	}
	op = f.call(t, "mkdir", mkdirArgs{Path: loc("created", "denied"), Parents: true})
	if op.Error == nil || op.Error.Code != "permission_denied" || op.Error.Effect != "none" {
		t.Fatalf("mkdir admission changed files before checking destination: %+v", op)
	}
	if _, err := os.Stat(filepath.Join(f.root, "created")); !os.IsNotExist(err) {
		t.Fatal("created an ancestor before the denied level")
	}
}

// mkdir -p 的授权口径：谁被创建才查谁。write/curl -o 的补父目录便利逻辑
// 只允许文件时不得放大到容器目录（edit/mkdir/remove 同口径）。
func TestMkdirParentsDoesNotRequireGrantsOnExistingAncestors(t *testing.T) {
	f := setup(t)
	if err := os.Mkdir(filepath.Join(f.root, "held"), 0o700); err != nil {
		t.Fatal(err)
	}
	check := f.fs.cfg.Check
	f.fs.cfg.Check = func(ctx context.Context, call Call, path string, write bool) error {
		if write && filepath.Base(path) != "leaf" {
			return hosts.Fail("permission_denied", "write requires a grant on "+path)
		}
		return check(ctx, call, path, write)
	}
	// 只授缺失的 leaf 级：已存在的 held 与 root 不需要写授权。
	value[fsp.Entry](t, f.call(t, "mkdir", mkdirArgs{Path: loc("held", "leaf"), Parents: true}))
	if info, err := os.Stat(filepath.Join(f.root, "held", "leaf")); err != nil || !info.IsDir() {
		t.Fatal("leaf not created", err)
	}
	// 目标全部已存在、零创建：任何层级都不需要写授权（fs write 补父目录的便利路径）。
	value[fsp.Entry](t, f.call(t, "mkdir", mkdirArgs{Path: loc("held"), Parents: true, ExistOK: true}))
}

// 缺失层级的拒绝提示指向最小授权目标（最上层缺失级），且拒绝时零副作用。
func TestMkdirParentsPreflightNamesTopmostMissingLevel(t *testing.T) {
	f := setup(t)
	if err := os.Mkdir(filepath.Join(f.root, "base-dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	check := f.fs.cfg.Check
	f.fs.cfg.Check = func(ctx context.Context, call Call, path string, write bool) error {
		if write && filepath.Base(path) == "mid" {
			return hosts.Fail("permission_denied", "grant "+path)
		}
		return check(ctx, call, path, write)
	}
	op := f.call(t, "mkdir", mkdirArgs{Path: loc("base-dir", "mid", "leaf"), Parents: true})
	if op.Error == nil || op.Error.Code != "permission_denied" || op.Error.Effect != "none" {
		t.Fatalf("missing-level denial: %+v", op)
	}
	if !strings.Contains(op.Error.Message, filepath.Join("base-dir", "mid")) {
		t.Fatalf("hint did not name the topmost missing level: %+v", op.Error)
	}
	if _, err := os.Stat(filepath.Join(f.root, "base-dir", "mid")); !os.IsNotExist(err) {
		t.Fatal("created a level before the denied one")
	}
}
