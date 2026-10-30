package host

import (
	"testing"

	"github.com/veypi/aic-pod/libs/mcpx"
)

// 配置热加载的差异应用：文件来源的服务集合变化必须落到运行态，
// 进程级注册的服务不受影响。
func TestApplyServiceSetRegistersReplacesAndRemoves(t *testing.T) {
	c, _ := testClient(t)
	c.svc.mu.Lock()
	c.svc.fileServices = map[string]mcpx.Config{}
	c.svc.mu.Unlock()

	first := mcpx.Config{Command: "/bin/echo", Args: []string{"hi"}}
	if err := c.applyServiceSet(map[string]mcpx.Config{"fixture": first}); err != nil {
		t.Fatalf("apply add: %v", err)
	}
	if _, ok := c.svc.registry.Lookup("fixture"); !ok {
		t.Fatal("service was not registered as a vsh command")
	}

	// 姿态变化 = 配置变化：服务重装（no_sandbox 是第三方浏览器类服务的前提）。
	second := first
	second.NoSandbox = true
	if err := c.applyServiceSet(map[string]mcpx.Config{"fixture": second}); err != nil {
		t.Fatalf("apply replace: %v", err)
	}
	current := c.fileServices()
	if !current["fixture"].NoSandbox {
		t.Fatal("posture change did not land")
	}

	// 配置删除 → 服务摘除。
	if err := c.applyServiceSet(map[string]mcpx.Config{}); err != nil {
		t.Fatalf("apply remove: %v", err)
	}
	if _, ok := c.svc.registry.Lookup("fixture"); ok {
		t.Fatal("service was not removed")
	}
}

// 禁用的条目不进入运行态（内置项禁用走同一条路）。
func TestApplyServiceSetSkipsDisabled(t *testing.T) {
	c, _ := testClient(t)
	c.svc.mu.Lock()
	c.svc.fileServices = map[string]mcpx.Config{}
	c.svc.mu.Unlock()

	disabled := mcpx.Config{Command: "/bin/echo", Disabled: true}
	if err := c.applyServiceSet(map[string]mcpx.Config{"fixture": disabled}); err != nil {
		t.Fatalf("apply disabled: %v", err)
	}
	if _, ok := c.svc.registry.Lookup("fixture"); ok {
		t.Fatal("disabled service must not be registered")
	}
}
