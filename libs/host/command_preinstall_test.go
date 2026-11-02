package host

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestPreinstalledCommandPackages(t *testing.T) {
	resources := t.TempDir()
	for _, id := range preinstalledCommandPackages {
		dir := filepath.Join(resources, id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, id), []byte("shipped-"+id), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("AIC_SKILL_BIN_DIR", resources)
	c, _ := testClient(t)
	root, err := c.commandPackageRoot()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range preinstalledCommandPackages {
		data, err := os.ReadFile(filepath.Join(root, id, "bin", id))
		if err != nil || string(data) != "shipped-"+id {
			t.Fatalf("%s: preinstalled binary = %q, %v", id, data, err)
		}
		if !c.cmds.has(id) {
			t.Fatalf("%s: command not registered after startup preinstall", id)
		}
		if err := c.uninstallCommandPackage(id); err == nil {
			t.Fatalf("%s: preinstalled package was uninstalled", id)
		}
		if err := c.installCommandPackage(context.Background(), id); err == nil {
			t.Fatalf("%s: preinstalled package was overwritten by commands install", id)
		}
	}
	for _, row := range c.hostCommandRows() {
		if preinstalledCommandPackage(row.Name) && !row.Preinstalled {
			t.Fatalf("%s: commands list row not marked preinstalled: %+v", row.Name, row)
		}
	}

	// 启动定版 = 无条件刷成内嵌版本：改过的包内容被还原，附带的二进制更新。
	if err := os.WriteFile(filepath.Join(root, "browser", "local-edit.txt"), []byte("local"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(resources, "browser", "browser"), []byte("shipped-v2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := c.preinstallCommandPackages(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "browser", "local-edit.txt")); !os.IsNotExist(err) {
		t.Fatal("local edit survived startup pinning")
	}
	data, err := os.ReadFile(filepath.Join(root, "browser", "bin", "browser"))
	if err != nil || string(data) != "shipped-v2" {
		t.Fatalf("binary after re-pin = %q, %v", data, err)
	}
	state, err := c.loadCommandPackageState()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range preinstalledCommandPackages {
		record := state.Packages[id]
		if !record.Builtin || record.Revision != 2 {
			t.Fatalf("%s: record = %+v, want builtin revision 2", id, record)
		}
	}
}

func TestPreinstallWithoutShippedBinariesKeepsSource(t *testing.T) {
	t.Setenv("AIC_SKILL_BIN_DIR", filepath.Join(t.TempDir(), "missing"))
	c, _ := testClient(t)
	root, err := c.commandPackageRoot()
	if err != nil {
		t.Fatal(err)
	}
	// 源码运行形态：没有附带二进制时包与命令仍注册，bin/ 不存在，
	// 调用时报明确的程序缺失，而不是静默成功。
	if !c.cmds.has("browser") || !c.cmds.has("cua") {
		t.Fatal("preinstalled commands missing without shipped binaries")
	}
	if _, err := os.Stat(filepath.Join(root, "browser", "bin")); !os.IsNotExist(err) {
		t.Fatal("bin/ appeared without shipped binaries")
	}
	if _, err := os.Stat(filepath.Join(root, "browser", "src")); err != nil {
		t.Fatalf("package source missing: %v", err)
	}
}
