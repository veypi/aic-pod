package host

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/mcpx"
	rtcwire "github.com/veypi/aic-pod/protocol/hosts_rtc"
	wire "github.com/veypi/aic-pod/protocol/tool"
)

func TestBuiltinMCPDefaultsAndOverrides(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AIC_AGENT_BROWSER_PATH", "")
	t.Setenv("AIC_AGENT_BROWSER_BUNDLE_DIR", filepath.Join(root, "agent-browser"))
	t.Setenv("AIC_CUA_DRIVER_PATH", filepath.Join(root, "cua-driver"))
	servers, err := mcpServers(nil, root)
	if err != nil {
		t.Fatal(err)
	}
	binary := "agent-browser"
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	if servers["browser"].Command != filepath.Join(root, "agent-browser", binary) || !reflect.DeepEqual(servers["browser"].Args, []string{"mcp", "--tools", "core,tabs"}) {
		t.Fatalf("browser must launch upstream directly: %+v", servers["browser"])
	}
	if servers["cua"].Command != filepath.Join(root, "cua-driver") || !reflect.DeepEqual(servers["cua"].Args, []string{"mcp"}) {
		t.Fatalf("CUA must launch upstream directly: %+v", servers["cua"])
	}
	configured := map[string]mcpx.Config{
		"browser": {Disabled: true},
		"cua":     {Command: "custom-cua"},
		"custom":  {URL: "https://example.test/mcp"},
	}
	servers, err = mcpServers(configured, root)
	if err != nil || !reflect.DeepEqual(servers, configured) {
		t.Fatalf("owner overrides changed: %+v, %v", servers, err)
	}
	// Disabled builtins require no command path and never start a process.
	if _, err := optionsOf(cfg.Options{MCP: mcpx.Settings{Servers: configured}}, "cli", "test", t.Logf); err != nil {
		t.Fatal(err)
	}
	c, _ := testClient(t)
	if err := c.configureMCP(mcpx.Settings{Servers: configured}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.mcpServices.Session(context.Background(), "browser"); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("disabled builtin started: %v", err)
	}
}

func callTool(t *testing.T, c *Client, ctx context.Context, caller wire.Caller, r wire.Request) wire.Response {
	t.Helper()
	r.Protocol = rtcwire.Protocol
	return c.HandleTool(ctx, caller, r)
}
func callNATS(t *testing.T, c *Client, subject string, raw []byte) wire.Response {
	t.Helper()
	return c.HandleNATS(context.Background(), subject, raw)
}
func TestMCPHasNoPlatformServer(t *testing.T) {
	saved := cfg.Global
	cfg.Global = cfg.NewOptions()
	cfg.Global.ExecPolicy = cfg.PolicyOpen
	t.Cleanup(func() { cfg.Global = saved })
	c, _ := testClient(t)
	response := callTool(t, c, context.Background(), testCaller(), execRequest("mcp tools aic", 30000))
	out := decoded[wire.ExecResult](t, response.Result)
	if response.Error != nil || out.Attrs["exit_code"] == "0" || !strings.Contains(out.Attrs["stderr"], "aic") {
		t.Fatalf("unexpected unconfigured service result: %+v %+v", response, out)
	}
	// The normal platform path still works with no configured MCP services.
	if result := callTool(t, c, context.Background(), testCaller(), fsRequest("roots", map[string]any{})); result.Error != nil {
		t.Fatal(result.Error)
	}
}

func TestMCPConfiguredServiceFromNativeExecAndUI(t *testing.T) {
	saved := cfg.Global
	cfg.Global = cfg.NewOptions()
	cfg.Global.ExecPolicy, cfg.Global.NetPolicy = cfg.PolicyOpen, cfg.PolicyOpen
	t.Cleanup(func() { cfg.Global = saved })
	var calls atomic.Int32
	service := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	mcp.AddTool(service, &mcp.Tool{Name: "next", Description: "Increment service state"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
		return nil, map[string]any{"count": calls.Add(1)}, nil
	})
	started, cancelled := make(chan struct{}), make(chan struct{})
	mcp.AddTool(service, &mcp.Tool{Name: "wait", Description: "Wait until cancelled"}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		close(started)
		select {
		case <-ctx.Done():
			close(cancelled)
			return nil, nil, ctx.Err()
		case <-time.After(10 * time.Second):
			return nil, nil, context.DeadlineExceeded
		}
	})
	endpoint := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return service }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, PropagateRequestCancellation: true}))
	defer endpoint.Close()
	c, _ := testClient(t)
	if err := c.configureMCP(mcpx.Settings{Servers: map[string]mcpx.Config{"fixture": {URL: endpoint.URL}}}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []int{1, 2, 3} {
		caller := testCaller()
		caller.Direct = want == 3 // UI uses the same exec path with a direct RTC caller.
		response := callTool(t, c, context.Background(), caller, execRequest("mcp call fixture next --json", 30000))
		output := decoded[wire.ExecResult](t, response.Result)
		var result mcp.CallToolResult
		if response.Error != nil || output.Attrs["exit_code"] != "0" || json.Unmarshal([]byte(output.Content), &result) != nil {
			t.Fatalf("native exec service call: %+v %+v", response, output)
		}
		var value struct {
			Count int `json:"count"`
		}
		raw, err := json.Marshal(result.StructuredContent)
		if err == nil {
			err = json.Unmarshal(raw, &value)
		}
		if err != nil || value.Count != want {
			t.Fatalf("state lost: %+v %v", value, err)
		}
	}
	// The UI's native cancel reaches the SDK call without closing the service.
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	caller := testCaller()
	caller.Direct = true
	request := execRequest("mcp call fixture wait --json", 30000)
	finished := make(chan wire.Response, 1)
	go func() { finished <- callTool(t, c, ctx, caller, request) }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("command did not reach MCP service")
	}
	response := callTool(t, c, ctx, caller, wire.Request{ID: "cancel_mcp", Action: wire.ActionCancel, CancelID: request.ID})
	if response.Error != nil {
		t.Fatal(response.Error)
	}
	select {
	case <-finished:
	case <-ctx.Done():
		t.Fatal("native cancellation did not stop command")
	}
	select {
	case <-cancelled:
	case <-ctx.Done():
		t.Fatal("native cancellation did not reach MCP service")
	}
	response = callTool(t, c, ctx, caller, execRequest("mcp call fixture next --json", 30000))
	out := decoded[wire.ExecResult](t, response.Result)
	if response.Error != nil || out.Attrs["exit_code"] != "0" || calls.Load() != 4 {
		t.Fatalf("service unusable after cancellation: %+v %+v", response, out)
	}
}

func TestBrowserStreamUsesExistingCommandAuthorization(t *testing.T) {
	saved := cfg.Global
	cfg.Global = cfg.NewOptions()
	t.Cleanup(func() { cfg.Global = saved })
	c, _ := testClient(t)
	caller := testCaller()
	call := func(caller wire.Caller) {
		t.Helper()
		err := c.streamBrowser(context.Background(), caller, nil, func([]byte) error { t.Fatal("unauthorized frame"); return nil })
		if err == nil || wire.AsFault(err).Code != "permission_denied" {
			t.Fatalf("browser bypassed command gate: %v", err)
		}
	}
	call(caller)
	caller.Direct = true
	caller.Scope = "fs"
	call(caller)
	caller.Scope = ""
	cfg.Global.ExecRules = []string{"deny:mcp.browser"}
	call(caller)
}
