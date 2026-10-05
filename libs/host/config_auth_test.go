package host

import (
	"context"
	"github.com/veypi/aic-pod/protocol"
	"os"
	"path/filepath"
	"testing"

	"github.com/veypi/aic-pod/cfg"
)

// TestMalformedAuthFailsClosedUntilRestart 锁定授权配置损坏的 fail-closed
// 语义：启动配置无效时 permissionState 带 invalid，RTC（HandleTool）与
// NATS（signedCall）两入口一律拒绝（修复 = 改配置 + 重启，2026-10-06 §4：
// cfg.Global 只是启动参数，运行中修改不再即时生效）。
func TestMalformedAuthFailsClosedUntilRestart(t *testing.T) {
	saved := cfg.Global
	t.Cleanup(func() { cfg.Global = saved })
	for _, corrupt := range []func(*cfg.Options){
		func(o *cfg.Options) { o.ExecPolicy = "dney" },
		func(o *cfg.Options) { o.ExecRules = []string{"fixture", "bad rule"} },
		func(o *cfg.Options) { o.FsRules = []string{"rw:/private", "rw:**"} },
		func(o *cfg.Options) { o.NetRules = []string{"allow:example.com", "allow:*:443"} },
	} {
		cfg.Global = cfg.NewOptions()
		cfg.Global.FsPolicy = cfg.PolicyOpen
		corrupt(cfg.Global)
		cfg.Global.Normalize()
		c, _ := testClient(t)
		if c.perms.err() == nil {
			t.Fatal("corrupt startup config not recorded")
		}
		path := filepath.Join(c.options().WorkDir, "keep.txt")
		if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(c.options().WorkDir, "marker.txt")
		requests := []protocol.Request{
			execRequest("echo ran > "+filepath.ToSlash(marker), 30000),
			fsRequest("text.write", map[string]any{"path": filepath.ToSlash(path), "content": "changed"}),
		}
		for _, request := range requests {
			for _, response := range []protocol.Response{callTool(t, c, context.Background(), testCaller(), request), signedCall(t, c, request, false, "s1", "")} {
				if response.Error == nil || response.Error.Code != "permission_denied" {
					t.Fatalf("damaged authorization admitted call: %+v", response)
				}
			}
		}
		data, err := os.ReadFile(path)
		if err != nil || string(data) != "original" {
			t.Fatal("rejected calls had side effects", err)
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatal("rejected exec had side effects", err)
		}
		c.Close()
	}
	// 修复 = 有效配置 + 重启（新客户端）
	cfg.Global = cfg.NewOptions()
	cfg.Global.FsPolicy = cfg.PolicyOpen
	c, _ := testClient(t)
	if c.perms.err() != nil {
		t.Fatal("valid startup config still gated")
	}
	path := filepath.Join(c.options().WorkDir, "keep.txt")
	if response := callTool(t, c, context.Background(), testCaller(), execRequest("echo ok", 30000)); response.Error != nil {
		t.Fatalf("repair did not restore exec: %+v", response)
	}
	if response := callTool(t, c, context.Background(), testCaller(), fsRequest("text.write", map[string]any{"path": filepath.ToSlash(path), "content": "changed"})); response.Error != nil {
		t.Fatalf("repair did not restore file calls: %+v", response)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "changed" {
		t.Fatal("repaired file call did not write", err)
	}
}
