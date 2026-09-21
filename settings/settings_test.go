package settings

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/veypi/aic-pod/cfg"
)

func isolateConfigDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv("APPDATA", dir)
	return dir
}

func writeRawConfig(t *testing.T, body string) {
	t.Helper()
	p, err := cfg.Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestApplyListSemantics：授权列表 nil = 保持现状，空数组 = 整体清空
// （空数组是 grant --permanent 的唯一回撤出口，不能被无关保存清掉）。
func TestApplyListSemantics(t *testing.T) {
	isolateConfigDir(t)
	writeRawConfig(t, "fs_policy: deny\nfs_allow: [/workspace]\nssh_allow: [a:22]\n")
	// nil 列表：不动现有清单
	if err := (&Update{FsPolicy: "deny"}).Apply(); err != nil {
		t.Fatal(err)
	}
	o, err := cfg.LoadFile()
	if err != nil || len(o.FsAllow) != 1 || o.FsAllow[0] != "/workspace" || len(o.SshAllow) != 1 {
		t.Fatalf("nil lists must keep current entries: %+v (%v)", o, err)
	}
	// 空数组：整体清空
	empty := []string{}
	if err := (&Update{FsAllow: &empty}).Apply(); err != nil {
		t.Fatal(err)
	}
	o, err = cfg.LoadFile()
	if err != nil || len(o.FsAllow) != 0 {
		t.Fatalf("empty list must clear entries: %+v (%v)", o, err)
	}
}

// TestApplyWorkDirExpandsHome：work_dir 的 ~ 展开并落盘为绝对路径。
func TestApplyWorkDirExpandsHome(t *testing.T) {
	dir := isolateConfigDir(t)
	writeRawConfig(t, "host: https://ivec-ai.com\n")
	target := filepath.Join(dir, "work")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := (&Update{WorkDir: "~/work"}).Apply(); err != nil {
		t.Fatal(err)
	}
	o, err := cfg.LoadFile()
	if err != nil || o.WorkDir != target {
		t.Fatalf("work_dir = %q, want %q (%v)", o.WorkDir, target, err)
	}
}

// TestBoundHostID：凭证首段即 host_id（4 段格式）。
func TestBoundHostID(t *testing.T) {
	if got := BoundHostID("abcdef.0011.2233.4455"); got != "abcdef" {
		t.Fatalf("BoundHostID = %q", got)
	}
	if got := BoundHostID("not-a-credential"); got != "" {
		t.Fatalf("malformed credential must yield empty host_id, got %q", got)
	}
}
