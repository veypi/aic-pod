package host

import (
	"context"
	"io"
	"runtime"
	"testing"
	"time"

	"github.com/veypi/aic-pod/libs/mcpx"
	"github.com/veypi/vsh/commands"
)

func managerHasService(c *Client, alias string) bool {
	manager := c.mcpManager()
	if manager == nil {
		return false
	}
	for _, status := range manager.Status() {
		if status.Name == alias {
			return true
		}
	}
	return false
}

// startWhileRegistrationLocked 让变更在登记锁被占用时启动。返回前给 goroutine
// 一次运行机会；正确实现不得提前修改 manager/owner/registry，也不得返回。
func startWhileRegistrationLocked(t *testing.T, c *Client, fn func() error) (done <-chan error, unlock func()) {
	t.Helper()
	c.svc.registrationMu.Lock()
	started := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		close(started)
		result <- fn()
	}()
	<-started
	runtime.Gosched()
	select {
	case err := <-result:
		c.svc.registrationMu.Unlock()
		t.Fatalf("service mutation bypassed registration lock: %v", err)
	case <-time.After(10 * time.Millisecond):
	}
	locked := true
	t.Cleanup(func() {
		if locked {
			c.svc.registrationMu.Unlock()
		}
	})
	return result, func() {
		c.svc.registrationMu.Unlock()
		locked = false
	}
}

func waitServiceMutation(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("service mutation did not finish after registration unlock")
	}
}

func TestMCPAddWaitsForWholeRegistrationTransaction(t *testing.T) {
	c, _ := testClient(t)
	const alias = "concurrent-add"
	inv := &commands.Invocation{Stdout: io.Discard, Stderr: io.Discard}
	done, unlock := startWhileRegistrationLocked(t, c, func() error {
		return c.mcpAdd(context.Background(), inv, []string{alias, "--url", "https://example.test/mcp"})
	})
	if managerHasService(c, alias) || c.serviceRegistry().Has(alias) || c.ownerConfigured(alias) {
		t.Fatal("add changed manager, registry or owner before acquiring registration lock")
	}
	unlock()
	waitServiceMutation(t, done)
	if !managerHasService(c, alias) || !c.serviceRegistry().Has(alias) || !c.ownerConfigured(alias) {
		t.Fatal("add did not publish a complete service after registration unlock")
	}
}

func TestMCPRemoveWaitsForWholeRegistrationTransaction(t *testing.T) {
	c, _ := testClient(t)
	const alias = "concurrent-remove"
	inv := &commands.Invocation{Stdout: io.Discard, Stderr: io.Discard}
	if err := c.mcpAdd(context.Background(), inv, []string{alias, "--url", "https://example.test/mcp"}); err != nil {
		t.Fatal(err)
	}
	done, unlock := startWhileRegistrationLocked(t, c, func() error {
		return c.mcpRemove(context.Background(), inv, []string{alias})
	})
	if !managerHasService(c, alias) || !c.serviceRegistry().Has(alias) || !c.ownerConfigured(alias) {
		t.Fatal("remove changed manager, registry or owner before acquiring registration lock")
	}
	unlock()
	waitServiceMutation(t, done)
	if managerHasService(c, alias) || c.serviceRegistry().Has(alias) || c.ownerConfigured(alias) {
		t.Fatal("remove left a partial service after registration unlock")
	}
}

func TestConfigureMCPWaitsForWholeRegistrationTransaction(t *testing.T) {
	c, _ := testClient(t)
	const alias = "configure-race"
	inv := &commands.Invocation{Stdout: io.Discard, Stderr: io.Discard}
	if err := c.mcpAdd(context.Background(), inv, []string{alias, "--url", "https://example.test/mcp"}); err != nil {
		t.Fatal(err)
	}
	done, unlock := startWhileRegistrationLocked(t, c, func() error {
		return c.configureMCP(mcpx.Settings{})
	})
	if !managerHasService(c, alias) || !c.serviceRegistry().Has(alias) || !c.ownerConfigured(alias) {
		t.Fatal("configure changed manager, registry or owner before acquiring registration lock")
	}
	unlock()
	waitServiceMutation(t, done)
	if managerHasService(c, alias) || c.serviceRegistry().Has(alias) || c.ownerConfigured(alias) {
		t.Fatal("configure left the removed runtime service partially registered")
	}
}
