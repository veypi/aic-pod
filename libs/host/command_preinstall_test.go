package host

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/veypi/aic-pod/cfg"
)

func TestPreinstalledCommandPackages(t *testing.T) {
	resources := t.TempDir()
	for _, id := range cfg.PreinstalledCommandPackages {
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
	for _, id := range cfg.PreinstalledCommandPackages {
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
	for _, id := range cfg.PreinstalledCommandPackages {
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

// 预装包的默认沙箱姿态：Windows 上 Chrome/CuaDriver 与受限令牌沙箱不兼容，
// 安装即视为同意其免沙箱运行；owner 显式定过的定义不动。
func TestPreinstalledSandboxDefaults(t *testing.T) {
	c, _ := testClient(t)
	restore := sandboxDefaultPlatforms
	sandboxDefaultPlatforms = map[string]bool{"windows": true, runtime.GOOS: true}
	t.Cleanup(func() { sandboxDefaultPlatforms = restore })

	ids := []string{"skill:browser:browser", "skill:cua:cua"}
	if err := c.applyPreinstalledSandboxDefaults(ids); err != nil {
		t.Fatal(err)
	}
	fileCfg, err := cfg.LoadFile()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if !fileCfg.CommandConfig[id].NoSandbox {
			t.Fatalf("%s: no_sandbox not defaulted: %+v", id, fileCfg.CommandConfig[id])
		}
	}

	// 幂等：再跑一次不改变已有配置。
	if err := c.applyPreinstalledSandboxDefaults(ids); err != nil {
		t.Fatal(err)
	}
	fileCfg, err = cfg.LoadFile()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if !fileCfg.CommandConfig[id].NoSandbox {
			t.Fatalf("%s: default not idempotent: %+v", id, fileCfg.CommandConfig[id])
		}
	}

	// owner 显式关掉（yaml 里写死 no_sandbox: false）→ 不被改回。
	fileCfg.CommandConfig["skill:browser:browser"] = cfg.CommandDefConfig{NoSandbox: false}
	if err := cfg.Save(fileCfg); err != nil {
		t.Fatal(err)
	}
	if err := c.applyPreinstalledSandboxDefaults(ids); err != nil {
		t.Fatal(err)
	}
	fileCfg, err = cfg.LoadFile()
	if err != nil {
		t.Fatal(err)
	}
	if fileCfg.CommandConfig["skill:browser:browser"].NoSandbox {
		t.Fatal("owner-disabled no_sandbox was overwritten")
	}
	if !fileCfg.CommandConfig["skill:cua:cua"].NoSandbox {
		t.Fatal("cua default lost")
	}

	// 非目标平台：不写默认姿态。
	sandboxDefaultPlatforms = map[string]bool{}
	fileCfg.CommandConfig = nil
	if err := cfg.Save(fileCfg); err != nil {
		t.Fatal(err)
	}
	if err := c.applyPreinstalledSandboxDefaults(ids); err != nil {
		t.Fatal(err)
	}
	fileCfg, err = cfg.LoadFile()
	if err != nil {
		t.Fatal(err)
	}
	if len(fileCfg.CommandConfig) != 0 {
		t.Fatalf("defaults written on unsupported platform: %+v", fileCfg.CommandConfig)
	}
}
