//go:build windows

package fsauth

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/veypi/vbox"
)

// TestDefaultDenyTableWithoutHome：Windows 的 GUI/schtasks 启动进程没有 HOME
// （只有 USERPROFILE）。内置 deny 表必须在这种环境下也能编译，且设备状态条目
// 仍落在真实家目录上——2026-10-04 win 实机故障：rule 30: cannot expand
// "$HOME/.aic/browser/**" 使整表被拒、host 无法启动（进程活着但连不上平台）。
func TestDefaultDenyTableWithoutHome(t *testing.T) {
	prev, had := os.LookupEnv("HOME")
	if err := os.Unsetenv("HOME"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if had {
			os.Setenv("HOME", prev)
		}
	})
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("no home dir: %v", err)
	}

	rows := defaultDenyPaths()
	for i := range rows {
		rows[i] = "deny:" + rows[i]
	}
	if _, err := vbox.CompileFSRules(rows, vbox.ClassBuiltin); err != nil {
		t.Fatalf("built-in deny table must compile with HOME unset: %v", err)
	}

	p := newTestPolicy(t, "")
	stateDir := filepath.ToSlash(home) + "/.aic"
	if !denied(p, stateDir+"/browser/browser.json") {
		t.Errorf("deniedPath(%q/…) = false, want true (device state must stay denied)", stateDir)
	}
	if !denied(p, stateDir+"/config.yaml") {
		t.Errorf("deniedPath(%q/config.yaml) = false, want true", stateDir)
	}
	if denied(p, stateDir+"/sessions/s1/x.txt") {
		t.Error("device state deny must not shadow session files")
	}
}
