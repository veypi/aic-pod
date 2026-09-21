//go:build windows

package hostfs

import (
	"os"
	"path/filepath"
	"testing"
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
	if err := os.Rename(filepath.Join(dir, "to"), filepath.Join(dir, "pinned")); err != nil {
		t.Fatal(err)
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
