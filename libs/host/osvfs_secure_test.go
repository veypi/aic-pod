package host

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/veypi/vbox"
)

func TestHostCheckedOpenPreservesNullDevice(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX device")
	}
	f, err := (OSVFS{}).OpenChecked("/dev/null", os.O_WRONLY|os.O_TRUNC, 0600, func(string, vbox.FileOp) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write([]byte("discard")); err != nil {
		t.Fatal(err)
	}
}

func TestHostCheckedOpenExclusiveAndRace(t *testing.T) {
	v := OSVFS{}
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	secret := filepath.Join(dir, "secret")
	os.WriteFile(file, []byte("keep"), 0600)
	os.WriteFile(secret, []byte("secret"), 0600)
	allow := func(string, vbox.FileOp) error { return nil }
	if _, err := v.OpenChecked(hostCanonical(file), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600, allow); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("exclusive: %v", err)
	}
	data, _ := os.ReadFile(file)
	if string(data) != "keep" {
		t.Fatal("exclusive open truncated file")
	}
	swapped := false
	_, err := v.OpenChecked(hostCanonical(file), os.O_TRUNC|os.O_WRONLY, 0600, func(string, vbox.FileOp) error {
		if !swapped {
			swapped = true
			os.Remove(file)
			if err := os.Symlink(secret, file); err != nil {
				t.Skip(err)
			}
		}
		return nil
	})
	if err == nil {
		t.Fatal("symlink replacement accepted")
	}
	data, _ = os.ReadFile(secret)
	if string(data) != "secret" {
		t.Fatal("secret truncated")
	}
}

func TestHostPinnedRenameSurvivesParentReplacement(t *testing.T) {
	dir := t.TempDir()
	parent := filepath.Join(dir, "target")
	moved := filepath.Join(dir, "moved")
	outside := filepath.Join(dir, "outside")
	os.Mkdir(parent, 0700)
	os.Mkdir(outside, 0700)
	os.WriteFile(filepath.Join(parent, "temp"), []byte("ok"), 0600)
	swapped := false
	err := (OSVFS{}).RenameChecked(hostCanonical(filepath.Join(parent, "temp")), hostCanonical(filepath.Join(parent, "final")), func(string, vbox.FileOp) error {
		if !swapped {
			swapped = true
			if err := os.Rename(parent, moved); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, parent); err != nil {
				t.Skip(err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside, "final")); !os.IsNotExist(err) {
		t.Fatal("write followed replacement parent")
	}
	data, err := os.ReadFile(filepath.Join(moved, "final"))
	if err != nil || string(data) != "ok" {
		t.Fatalf("%q %v", data, err)
	}
}
