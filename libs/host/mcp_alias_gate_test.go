package host

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/protocol"
)

func TestMCPDebugAndLifecycleUseAliasGate(t *testing.T) {
	saved := cfg.Global
	cfg.Global = cfg.NewOptions()
	cfg.Global.ExecPolicy = cfg.PolicyDeny
	cfg.Global.NetPolicy = cfg.PolicyOpen
	t.Cleanup(func() { cfg.Global = saved })

	service := mcp.NewServer(&mcp.Implementation{Name: "gate-fixture", Version: "1"}, nil)
	mcp.AddTool(service, &mcp.Tool{Name: "next"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
		return nil, map[string]any{"ok": true}, nil
	})
	endpoint := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return service }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true}))
	defer endpoint.Close()

	c, _ := testClient(t)
	c.perms.grantCmd("s1", "mcp") // Management only; deliberately no mcp.fixture grant.
	run := func(script string) protocol.Output {
		t.Helper()
		r := callTool(t, c, context.Background(), testCaller(), execRequest(script, 30000))
		return decoded[protocol.Output](t, r.Result)
	}
	if out := run("mcp add fixture --url " + endpoint.URL); out.Attrs["exit_code"] != "0" {
		t.Fatalf("fixture setup failed: %+v", out)
	}
	if c.execAllowed("s1", "mcp.fixture") {
		t.Fatal("test setup unexpectedly granted mcp.fixture")
	}
	if out := run("fixture next --json"); out.Attrs["exit_code"] != "126" {
		t.Fatalf("direct alias gate did not deny: %+v", out)
	}
	for _, script := range []string{
		"mcp tools fixture",
		"mcp describe fixture next",
		"mcp call fixture next --json",
		"mcp read fixture file:///probe",
		"mcp stop fixture",
		"mcp start fixture",
		"mcp restart fixture",
	} {
		out := run(script)
		if out.Attrs["exit_code"] != "126" || !strings.Contains(out.Attrs["stderr"], "mcp.fixture") {
			t.Errorf("%s bypassed missing alias gate: %+v", script, out)
		}
	}

	c.perms.grantCmd("s1", "mcp.fixture")
	for _, script := range []string{"fixture next --json", "mcp call fixture next --json"} {
		if out := run(script); out.Attrs["exit_code"] != "0" {
			t.Errorf("authorized call %s failed: %+v", script, out)
		}
	}
}
