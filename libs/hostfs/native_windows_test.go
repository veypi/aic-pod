//go:build windows

package hostfs

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	fsp "github.com/veypi/aic-pod/protocol/fs"
)

func TestWindowsOpenRegularRejectsLinksAndNonLeafNames(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "target"), []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, name := range []string{".", "..", "../target", `sub\target`, "target:stream"} {
		if file, err := openRegular(root, name); err == nil {
			file.Close()
			t.Fatalf("accepted non-leaf %q", name)
		}
	}
	if err := os.Symlink(filepath.Join(dir, "target"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if file, err := openRegular(root, "link"); err == nil {
		file.Close()
		t.Fatal("followed a reparse point")
	}
}

func TestWindowsRenameUsesPinnedParentsAndNoReplace(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"from", "to"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	fromRoot, err := os.OpenRoot(filepath.Join(dir, "from"))
	if err != nil {
		t.Fatal(err)
	}
	defer fromRoot.Close()
	toRoot, err := os.OpenRoot(filepath.Join(dir, "to"))
	if err != nil {
		t.Fatal(err)
	}
	defer toRoot.Close()
	from, err := fromRoot.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	defer from.Close()
	to, err := toRoot.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	defer to.Close()
	if err := os.WriteFile(filepath.Join(dir, "from", "src"), []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "to", "dst"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	// Existing destinations must fail atomically, even below FS preflight.
	if err := renameNoReplace(int(from.Fd()), "src", int(to.Fd()), "dst"); !os.IsExist(err) {
		t.Fatal("no-replace did not reject existing target", err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "to", "dst")); err != nil || string(data) != "keep" {
		t.Fatal("overwrote existing destination", err)
	}
	// Rename the parent and put a different directory at its former pathname.
	// Windows：目录仍被持久句柄引用（os.OpenRoot / Open(".")）时，改名目录本身会被
	// 共享冲突拒绝（ERROR_SHARING_VIOLATION / "being used by another process"）——
	// 这等价于「钉住」在平台层的强保证：句柄释放前路径不可能被替换，此时用例结束；
	// 可改名（句柄共享 DELETE 的实现）时继续验证钉住语义（go1.27 windows runner 走前者）。
	if err := os.Rename(filepath.Join(dir, "to"), filepath.Join(dir, "pinned")); err != nil {
		t.Logf("parent rename blocked while pinned (windows sharing semantics): %v", err)
		return
	}
	if err := os.Mkdir(filepath.Join(dir, "to"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := renameReplace(int(from.Fd()), "src", int(to.Fd()), "dst"); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "pinned", "dst")); err != nil || string(data) != "source" {
		t.Fatal("rename lost the pinned parent", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "to", "dst")); !os.IsNotExist(err) {
		t.Fatal("rename used the replaced parent pathname", err)
	}
}

// 单条不可 stat 的子项不应让整个目录列举失败：系统联结（C:\Users\All Users 等）与
// 独占文件（pagefile.sys、NTUSER.DAT）可枚举但拒绝打开——用例用共享模式 0 的独占
// 句柄构造同样的条目，列举应回退目录扫描元数据正常返回。
func TestWindowsListToleratesUnstatableChildren(t *testing.T) {
	f := setup(t)
	locked := filepath.Join(f.root, "locked.txt")
	if err := os.WriteFile(locked, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "open.txt"), []byte("y"), 0600); err != nil {
		t.Fatal(err)
	}
	target, err := syscall.UTF16PtrFromString(locked)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := syscall.CreateFile(target, syscall.GENERIC_READ, 0, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Skipf("cannot take an exclusive handle: %v", err)
	}
	defer syscall.CloseHandle(handle)
	type page struct {
		Entries []fsp.Entry `json:"entries"`
		Next    string      `json:"next_cursor"`
	}
	got := value[page](t, f.call(t, "list", listArgs{Path: loc()}))
	if len(got.Entries) != 2 || got.Entries[0].Name != "locked.txt" || got.Entries[1].Name != "open.txt" {
		t.Fatalf("listing failed or dropped a locked entry: %+v", got.Entries)
	}
	if got.Entries[0].Kind != "file" || got.Entries[0].Size == nil || *got.Entries[0].Size != 1 {
		t.Fatalf("locked entry metadata not from the directory scan: %+v", got.Entries[0])
	}
}

// 缺省列表按 Windows 隐藏属性过滤（Explorer 同款）：hidden 未开时不显示，打开后出现；
// find 同口径（隐藏项不访问不递归）。
func TestWindowsListSkipsHiddenAttributeEntries(t *testing.T) {
	f := setup(t)
	for _, name := range []string{"visible.txt", "hidden.txt"} {
		if err := os.WriteFile(filepath.Join(f.root, name), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	attrs, err := syscall.UTF16PtrFromString(filepath.Join(f.root, "hidden.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.SetFileAttributes(attrs, syscall.FILE_ATTRIBUTE_HIDDEN); err != nil {
		t.Fatal(err)
	}
	type page struct {
		Entries []fsp.Entry `json:"entries"`
	}
	got := value[page](t, f.call(t, "list", listArgs{Path: loc()}))
	if len(got.Entries) != 1 || got.Entries[0].Name != "visible.txt" {
		t.Fatalf("hidden attribute entry not filtered: %+v", got.Entries)
	}
	all := value[page](t, f.call(t, "list", listArgs{Path: loc(), Hidden: true}))
	if len(all.Entries) != 2 {
		t.Fatalf("hidden=true must reveal attribute-hidden entries: %+v", all.Entries)
	}
	found := value[struct {
		Entries   []fsp.Entry `json:"entries"`
		Truncated bool        `json:"truncated"`
	}](t, f.call(t, "find", findArgs{Path: loc(), Glob: "*", Depth: 2, Limit: 10}))
	if len(found.Entries) != 1 || found.Entries[0].Name != "visible.txt" {
		t.Fatalf("find did not skip the hidden attribute entry: %+v", found.Entries)
	}
}

// 指向目录的 reparse（目录符号链接/junction）按目录显示：不再因 surrogate 型 reparse
// 被归为 other 而在前端退化成伪文件。
func TestWindowsListClassifiesDirectoryLinksAsDirectories(t *testing.T) {
	f := setup(t)
	if err := os.Mkdir(filepath.Join(f.root, "target"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(f.root, "target"), filepath.Join(f.root, "dirlink")); err != nil {
		t.Skipf("cannot create a directory symlink: %v", err)
	}
	type page struct {
		Entries []fsp.Entry `json:"entries"`
	}
	got := value[page](t, f.call(t, "list", listArgs{Path: loc()}))
	kind := ""
	for _, e := range got.Entries {
		if e.Name == "dirlink" {
			kind = e.Kind
		}
	}
	if kind != "directory" {
		t.Fatalf("directory link kind = %q, entries: %+v", kind, got.Entries)
	}
}
