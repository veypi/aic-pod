package hosts_rtc

import (
	wire "github.com/veypi/aic-pod/protocol/tool"
	"testing"
)

func TestOnlyAuthenticationAndNativeFrames(t *testing.T) {
	native := &wire.Request{Protocol: Protocol, ID: "native", Action: wire.ActionExec, Exec: &wire.ExecPayload{Script: "mcp tools browser"}}
	for _, r := range []Request{{Tool: native}, {ID: "auth", Auth: "open", Ticket: "signed"}, {Tool: native, GrantApproved: true}} {
		if err := r.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range []Request{{}, {ID: "auth", Auth: "open", Ticket: "signed", Tool: native}, {Tool: native, Ticket: "signed"}, {GrantApproved: true}} {
		if r.Validate() == nil {
			t.Fatalf("mixed authority/payload admitted: %+v", r)
		}
	}
	var request Request
	if wire.Decode([]byte(`{"mcp":{"server":"fixture","message":{"jsonrpc":"2.0","id":1,"method":"tools/list"}}}`), &request) == nil {
		t.Fatal("RTC admitted a separate MCP protocol")
	}
}
