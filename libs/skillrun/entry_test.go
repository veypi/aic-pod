package skillrun

import (
	"os"
	"path/filepath"
	"testing"
)

// Windows 上 provider 产物带 .exe 而 manifest entry 不带后缀（2026-10-05 win 实机：
// skill 服务启动报 "exec: unknown action <无后缀路径>"）。
func TestResolveEntryWindowsExeSuffix(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "probe.exe"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := resolveEntryFor(dir, "probe", "windows")
	if err != nil {
		t.Fatalf("windows 应取 .exe 形态：%v", err)
	}
	if filepath.Base(got) != "probe.exe" {
		t.Fatalf("windows got %q, want probe.exe", got)
	}
	// 非 Windows 保持严格：原样不存在即失败（不掩盖清单写错）。
	if _, err := resolveEntryFor(dir, "probe", "darwin"); err == nil {
		t.Fatal("darwin 不应接受 .exe 回退")
	}

	// 旧安装残留的无后缀同名文件不可执行，Windows 上 .exe 必须优先。
	if err := os.WriteFile(filepath.Join(dir, "stale"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stale.exe"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := resolveEntryFor(dir, "stale", "windows"); err != nil || filepath.Base(got) != "stale.exe" {
		t.Fatalf("windows 应优先 .exe：got %q err %v", got, err)
	}

	// 只有原样形态（非 Windows 布局）时按原样。
	if err := os.WriteFile(filepath.Join(dir, "plain"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := resolveEntryFor(dir, "plain", "linux"); err != nil || filepath.Base(got) != "plain" {
		t.Fatalf("linux 原样：got %q err %v", got, err)
	}

	if _, err := resolveEntryFor(dir, "missing", "windows"); err == nil {
		t.Fatal("两形态都不存在时必须报错")
	}
}
