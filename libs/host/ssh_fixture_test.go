package host

import (
	"os"
	"path/filepath"
	"testing"
)

// writeSSHTestConfig 写入隔离 HOME 下的 ~/.ssh/config（集成测试夹具）。
func writeSSHTestConfig(t *testing.T, home, text string) string {
	t.Helper()
	p := filepath.Join(home, ".ssh", "config")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
