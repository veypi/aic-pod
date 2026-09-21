package host

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/veypi/aic-pod/cfg"
	tool "github.com/veypi/aic-pod/libs/hosts_tool"
	wire "github.com/veypi/aic-pod/protocol/hosts_tools"
)

func TestMalformedAuthBlocksRTCAndNATSUntilRepaired(t *testing.T) {
	saved := cfg.Global
	cfg.Global = cfg.NewOptions()
	t.Cleanup(func() { cfg.Global = saved })
	c, _ := testClient(t)
	path := filepath.Join(c.options().WorkDir, "keep.txt")
	if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	if err := c.tools.RegisterCommand(tool.DefineCommand("fixture", tool.Bind(tool.Spec{Name: "run", Access: 1}, func(context.Context, tool.Caller, struct{}) (bool, error) { calls++; return true, nil }))); err != nil {
		t.Fatal(err)
	}
	writeArgs, _ := json.Marshal(map[string]any{"path": filepath.ToSlash(path), "content": "changed"})
	requests := []wire.Request{
		{Action: "call", Call: &wire.Invocation{Domain: "exec", Command: "fixture", Method: "run", Args: json.RawMessage(`{}`)}},
		{Action: "call", Call: &wire.Invocation{Domain: "fs", Method: "text.write", Args: writeArgs}},
	}
	for _, corrupt := range []func(){
		func() { cfg.Global.ExecPolicy = "dney" },
		func() { cfg.Global.ExecDeny = []string{"fixture", "bad rule"} },
		func() { cfg.Global.FsDeny = []string{"/private", "ro:/secret"} },
		func() { cfg.Global.NetDeny = []string{"example.com", "*:443"} },
	} {
		cfg.Global = cfg.NewOptions()
		cfg.Global.FsPolicy = cfg.PolicyOpen
		corrupt()
		cfg.Global.Normalize()
		for _, request := range requests {
			request.ID = wire.NewID("r_")
			for _, response := range []wire.Response{c.HandleTool(context.Background(), testCaller(), request), signedCall(t, c, request, 9, "s1", "")} {
				if response.Error == nil || response.Error.Code != "permission_denied" {
					t.Fatalf("damaged authorization admitted call: %+v", response)
				}
			}
		}
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "original" || calls != 0 {
		t.Fatal("rejected calls had side effects", err)
	}
	cfg.Global = cfg.NewOptions()
	cfg.Global.FsPolicy = cfg.PolicyOpen
	request := requests[0]
	request.ID = wire.NewID("r_")
	if response := c.HandleTool(context.Background(), testCaller(), request); response.Error != nil || calls != 1 {
		t.Fatalf("repair did not restore calls: %+v", response)
	}
	request = requests[1]
	request.ID = wire.NewID("r_")
	if response := c.HandleTool(context.Background(), testCaller(), request); response.Error != nil {
		t.Fatalf("repair did not restore file calls: %+v", response)
	}
	data, err = os.ReadFile(path)
	if err != nil || string(data) != "changed" {
		t.Fatal("repaired file call did not write", err)
	}
}
