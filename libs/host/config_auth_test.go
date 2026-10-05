package host

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/veypi/aic-pod/cfg"
	wire "github.com/veypi/aic-pod/protocol/tool"
)

// TestMalformedAuthBlocksRTCAndNATSUntilRepaired 锁定授权配置损坏的
// fail-closed 语义：RTC（HandleTool）与 NATS（signedCall）两入口在配置
// 修复前一律拒绝，修复后恢复。
func TestMalformedAuthBlocksRTCAndNATSUntilRepaired(t *testing.T) {
	saved := cfg.Global
	cfg.Global = cfg.NewOptions()
	t.Cleanup(func() { cfg.Global = saved })
	c, _ := testClient(t)
	path := filepath.Join(c.options().WorkDir, "keep.txt")
	if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(c.options().WorkDir, "marker.txt")
	requests := []wire.Request{
		execRequest("echo ran > "+filepath.ToSlash(marker), 30000),
		fsRequest("text.write", map[string]any{"path": filepath.ToSlash(path), "content": "changed"}),
	}
	for _, corrupt := range []func(){
		func() { cfg.Global.ExecPolicy = "dney" },
		func() { cfg.Global.ExecRules = []string{"fixture", "bad rule"} },
		func() { cfg.Global.FsRules = []string{"rw:/private", "rw:**"} },
		func() { cfg.Global.NetRules = []string{"allow:example.com", "allow:*:443"} },
	} {
		cfg.Global = cfg.NewOptions()
		cfg.Global.FsPolicy = cfg.PolicyOpen
		corrupt()
		cfg.Global.Normalize()
		for _, request := range requests {
			request.ID = wire.NewID("r_")
			for _, response := range []wire.Response{callTool(t, c, context.Background(), testCaller(), request), signedCall(t, c, request, false, "s1", "")} {
				if response.Error == nil || response.Error.Code != "permission_denied" {
					t.Fatalf("damaged authorization admitted call: %+v", response)
				}
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
	cfg.Global = cfg.NewOptions()
	cfg.Global.FsPolicy = cfg.PolicyOpen
	if response := callTool(t, c, context.Background(), testCaller(), requests[0]); response.Error != nil {
		t.Fatalf("repair did not restore exec: %+v", response)
	}
	if response := callTool(t, c, context.Background(), testCaller(), requests[1]); response.Error != nil {
		t.Fatalf("repair did not restore file calls: %+v", response)
	}
	data, err = os.ReadFile(path)
	if err != nil || string(data) != "changed" {
		t.Fatal("repaired file call did not write", err)
	}
}
