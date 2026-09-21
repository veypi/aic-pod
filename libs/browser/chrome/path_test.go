package chrome

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProfileLockAndExplicitExecutable(t *testing.T) {
	dir := t.TempDir()
	unlock, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := Lock(dir); err == nil {
		second()
		t.Fatal("profile concurrently locked")
	}
	unlock()
	unlock, err = Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	exe := filepath.Join(dir, "chrome.exe")
	if err = os.WriteFile(exe, []byte("test"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AIC_BROWSER_DEFAULT_PATH", exe)
	t.Setenv("AIC_BROWSER_PATH", filepath.Join(dir, "missing"))
	if _, err = Resolve(""); err == nil {
		t.Fatal("invalid explicit path fell back silently")
	}
	if p, err := Resolve(exe); err != nil || p != exe {
		t.Fatalf("configured path precedence: %s %v", p, err)
	}
}
