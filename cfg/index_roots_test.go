package cfg

import (
	"os"
	"path/filepath"
	"testing"
)

func TestApplyIndexRootsExpandsAndReplaces(t *testing.T) {
	dir := isolateConfigDir(t)
	writeRawConfig(t, "index_roots: ['/old']\n")
	target := filepath.Join(dir, "indexed")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	roots := []string{"~/indexed"}
	if err := (&SettingsUpdate{IndexRoots: &roots}).Apply(); err != nil {
		t.Fatal(err)
	}
	o, err := LoadFile()
	if err != nil || len(o.IndexRoots) != 1 || o.IndexRoots[0] != target {
		t.Fatalf("index_roots = %v (%v)", o.IndexRoots, err)
	}

	empty := []string{}
	if err := (&SettingsUpdate{IndexRoots: &empty}).Apply(); err != nil {
		t.Fatal(err)
	}
	o, err = LoadFile()
	if err != nil || len(o.IndexRoots) != 0 {
		t.Fatalf("empty index_roots must restore work_dir default: %v (%v)", o.IndexRoots, err)
	}
}
