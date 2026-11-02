package host

import (
	"testing"

	"github.com/veypi/aic-pod/cfg"
)

// 配置热加载统一走 commands/command_config；运行期未落盘命令不受文件重载影响。
func TestReloadCommandConfigRegistersReplacesAndRemoves(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c, _ := testClient(t)
	first := cfg.CommandConfig{Run: []string{"/bin/echo"}, Protocol: "mcp"}
	if err := cfg.Save(&cfg.Options{Commands: map[string]cfg.CommandConfig{"fixture": first}}); err != nil {
		t.Fatal(err)
	}
	if err := c.reloadCommandConfig(); err != nil {
		t.Fatalf("reload add: %v", err)
	}
	if !c.cmds.has("fixture") {
		t.Fatal("configured MCP service was not registered as a vsh command")
	}
	c.cmds.mu.RLock()
	oldManager := c.cmds.managers["fixture"]
	c.cmds.mu.RUnlock()

	// 姿态变化 = 定义变化：只替换该命令的 manager。
	second := first
	second.NoSandbox = true
	if err := cfg.Save(&cfg.Options{Commands: map[string]cfg.CommandConfig{"fixture": second}}); err != nil {
		t.Fatal(err)
	}
	if err := c.reloadCommandConfig(); err != nil {
		t.Fatalf("reload replace: %v", err)
	}
	c.cmds.mu.RLock()
	newManager := c.cmds.managers["fixture"]
	def := c.cmds.active["fixture"]
	c.cmds.mu.RUnlock()
	if newManager == oldManager || !def.noSandbox() {
		t.Fatal("command posture change did not land")
	}

	if err := cfg.Save(&cfg.Options{}); err != nil {
		t.Fatal(err)
	}
	if err := c.reloadCommandConfig(); err != nil {
		t.Fatalf("reload remove: %v", err)
	}
	if c.cmds.has("fixture") {
		t.Fatal("removed configured command is still registered")
	}
}

func TestReloadCommandConfigPreservesRuntimeCommands(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c, _ := testClient(t)
	def := &hostCommand{
		ID: "config:runtime", Alias: "runtime", Source: commandSourceConfig,
		Config: cfg.CommandConfig{URL: "https://example.test/mcp", Protocol: "mcp"},
	}
	if err := c.cmds.upsertRuntime(def); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(&cfg.Options{}); err != nil {
		t.Fatal(err)
	}
	if err := c.reloadCommandConfig(); err != nil {
		t.Fatal(err)
	}
	if !c.cmds.has("runtime") {
		t.Fatal("file reload removed a process-level runtime command")
	}
}
