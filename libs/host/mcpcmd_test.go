package host

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/protocol"
)

// vtools 全生命周期：mcp add → alias 根命令直调（flag 映射 / 目录 / 参数表）
// → list / stop / start → remove → 命令消失；以及管理动词的 exec 规则门。
func TestVtoolsServiceLifecycle(t *testing.T) {
	saved := cfg.Global
	cfg.Global = cfg.NewOptions()
	cfg.Global.ExecPolicy, cfg.Global.NetPolicy = cfg.PolicyOpen, cfg.PolicyOpen
	t.Cleanup(func() { cfg.Global = saved })
	var calls atomic.Int32
	service := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	mcp.AddTool(service, &mcp.Tool{Name: "next", Description: "Increment"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
		return nil, map[string]any{"count": calls.Add(1)}, nil
	})
	mcp.AddTool(service, &mcp.Tool{Name: "greet", Description: "问候"}, func(_ context.Context, _ *mcp.CallToolRequest, in struct {
		Name string `json:"name"`
	}) (*mcp.CallToolResult, any, error) {
		return nil, map[string]any{"hello": in.Name}, nil
	})
	endpoint := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return service }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true}))
	defer endpoint.Close()
	c, _ := testClient(t)
	run := func(script string) (protocol.Output, protocol.Response) {
		t.Helper()
		r := callTool(t, c, context.Background(), testCaller(), execRequest(script, 30000))
		out := decoded[protocol.Output](t, r.Result)
		return out, r
	}
	exitCode := func(script string) string {
		out, _ := run(script)
		return out.Attrs["exit_code"]
	}
	content := func(script string) string {
		out, _ := run(script)
		return out.Content
	}

	// 未登记前：127（registry 收口，不落原生）
	if got := exitCode("fixture next"); got != "127" {
		t.Fatalf("unregistered alias: exit=%s", got)
	}
	// add → 登记为根命令（连接懒惰：add 不拨号）
	if got := exitCode("mcp add fixture --url " + endpoint.URL); got != "0" {
		out, r := run("mcp add fixture --url " + endpoint.URL)
		t.Fatalf("add: exit=%s %+v %+v", got, out, r)
	}
	// 直调：不经 mcp 命令
	out := content("fixture next --json")
	if exitCode("fixture next --json") != "0" || !strings.Contains(out, `"count":1`) {
		t.Fatalf("direct call: %q", out)
	}
	// flag 映射（greet.name 是 schema 标量）
	out = content("fixture greet --name veypi --json")
	if !strings.Contains(out, `"hello":"veypi"`) && !strings.Contains(out, `"hello": "veypi"`) {
		t.Fatalf("flag mapping: %q", out)
	}
	// 目录与参数表
	if out := content("fixture"); !strings.Contains(out, "next") || !strings.Contains(out, "greet") {
		t.Fatalf("catalog: %q", out)
	}
	if out := content("fixture greet --help"); !strings.Contains(out, "--name (string)") {
		t.Fatalf("tool help: %q", out)
	}
	// list 看到 running
	if out := content("mcp list"); !strings.Contains(out, "fixture\tmcp\trunning") {
		t.Fatalf("list: %q", out)
	}
	// stop → start（连接断开重建，服务状态延续）
	if got := exitCode("mcp stop fixture"); got != "0" {
		t.Fatalf("stop: %s", got)
	}
	if got := exitCode("mcp start fixture"); got != "0" {
		t.Fatalf("start: %s", got)
	}
	if out := content("fixture next --json"); !strings.Contains(out, `"count"`) {
		t.Fatalf("call after restart: %q", out)
	}
	// remove → 命令消失（127）
	if got := exitCode("mcp remove fixture"); got != "0" {
		t.Fatalf("remove: %s", got)
	}
	if got := exitCode("fixture next"); got != "127" {
		t.Fatalf("removed alias still resolves: exit=%s", got)
	}
	// 内置服务不可移除
	removed, _ := run("mcp remove browser")
	if removed.Attrs["exit_code"] != "2" || !strings.Contains(removed.Attrs["stderr"], "内置服务") {
		t.Fatalf("builtin removal: %+v", removed)
	}
}

// 管理动词与工具命令的门：deny 策略下 mcp add / alias 直调都是 126 + grant 引导。
func TestVtoolsGates(t *testing.T) {
	saved := cfg.Global
	cfg.Global = cfg.NewOptions()
	cfg.Global.ExecPolicy = cfg.PolicyDeny // 显式 deny（默认是 open）
	t.Cleanup(func() { cfg.Global = saved })
	c, _ := testClient(t)
	run := func(script string) protocol.Output {
		t.Helper()
		r := callTool(t, c, context.Background(), testCaller(), execRequest(script, 30000))
		return decoded[protocol.Output](t, r.Result)
	}
	out := run("mcp add fixture --url http://127.0.0.1:1/mcp")
	if out.Attrs["exit_code"] != "126" || !strings.Contains(out.Attrs["stderr"], "grant cmd mcp") {
		t.Fatalf("add gate: %+v", out)
	}
	// 内置 browser 的工具命令同样过 exec 规则门（mcp.browser）
	out = run("browser")
	if out.Attrs["exit_code"] != "126" || !strings.Contains(out.Attrs["stderr"], "mcp.browser") {
		t.Fatalf("alias gate: %+v", out)
	}
	// 放行后不再被门拦（连接失败是后话：不是 126）
	c.perms.grantCmd("s1", "mcp.browser")
	if out = run("browser"); out.Attrs["exit_code"] == "126" {
		t.Fatalf("granted alias still gated: %+v", out)
	}
	// mcp list 只读放行
	if out = run("mcp list"); out.Attrs["exit_code"] != "0" {
		t.Fatalf("list: %+v", out)
	}
}

// mcp add --tcp 拒收 --idle（TCP 连接懒惰，无空闲回收，收了只会静默误导）。
func TestMcpAddTCPRejectsIdle(t *testing.T) {
	saved := cfg.Global
	cfg.Global = cfg.NewOptions()
	cfg.Global.ExecPolicy, cfg.Global.NetPolicy = cfg.PolicyOpen, cfg.PolicyOpen
	t.Cleanup(func() { cfg.Global = saved })
	c, _ := testClient(t)
	r := callTool(t, c, context.Background(), testCaller(), execRequest("mcp add foo --tcp 127.0.0.1:9866 --idle 1m", 30000))
	out := decoded[protocol.Output](t, r.Result)
	if out.Attrs["exit_code"] != "2" || !strings.Contains(out.Attrs["stderr"], "--idle") {
		t.Fatalf("tcp --idle must be rejected: %+v", out)
	}
}

// commands 展示面包含已登记的服务 alias（动态 Discoverable）。
func TestVtoolsDiscoverable(t *testing.T) {
	saved := cfg.Global
	cfg.Global = cfg.NewOptions()
	cfg.Global.ExecPolicy, cfg.Global.NetPolicy = cfg.PolicyOpen, cfg.PolicyOpen
	t.Cleanup(func() { cfg.Global = saved })
	c, _ := testClient(t)
	out := func(script string) protocol.Output {
		r := callTool(t, c, context.Background(), testCaller(), execRequest(script, 30000))
		return decoded[protocol.Output](t, r.Result)
	}
	text := out("commands").Content
	if !strings.Contains(text, "browser") || !strings.Contains(text, "cua") {
		t.Fatalf("builtin services not discoverable: %q", text)
	}
	if strings.Contains(text, "sed") || strings.Contains(text, "grep") {
		t.Fatalf("builtins must stay out of the discovery view: %q", text)
	}
}
