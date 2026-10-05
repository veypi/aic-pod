package protocol

import (
	"testing"
)

func TestOnlyAuthenticationAndNativeFrames(t *testing.T) {
	native := &Request{Protocol: RtcProtocol, ID: "native", Action: ActionExec, Exec: &ExecPayload{Script: "mcp tools browser"}}
	for _, r := range []RtcRequest{{Tool: native}, {ID: "auth", Auth: "open", Ticket: "signed"}, {Tool: native, GrantApproved: true}} {
		if err := r.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range []RtcRequest{{}, {ID: "auth", Auth: "open", Ticket: "signed", Tool: native}, {Tool: native, Ticket: "signed"}, {GrantApproved: true}} {
		if r.Validate() == nil {
			t.Fatalf("mixed authority/payload admitted: %+v", r)
		}
	}
	var request RtcRequest
	if Decode([]byte(`{"mcp":{"server":"fixture","message":{"jsonrpc":"2.0","id":1,"method":"tools/list"}}}`), &request) == nil {
		t.Fatal("RTC admitted a separate MCP protocol")
	}
}
