package host

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/mcpx"
	"github.com/veypi/aic-pod/libs/vtools"
	"github.com/veypi/aic-pod/protocol"
	"github.com/veypi/vsh/commands"
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

// mcp add --tcp 拒收 --idle（TCP 连接懒惰，无空闲回收，收了只会静默误导）；
// --cwd/--env 仅对 command 形态有效（--tcp/--url 形态下收了会被静默丢弃）。
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
	r = callTool(t, c, context.Background(), testCaller(), execRequest("mcp add foo --tcp 127.0.0.1:9866 --env A=B", 30000))
	out = decoded[protocol.Output](t, r.Result)
	if out.Attrs["exit_code"] != "2" || !strings.Contains(out.Attrs["stderr"], "--cwd/--env") {
		t.Fatalf("tcp --env must be rejected: %+v", out)
	}
}

// 服务 alias 撞平台命令名（ssh/grant/…）：平台命令先占位，服务被拒登记并记
// 日志，pod 照常初始化（回归：原装配顺序服务先登记，撞名的平台命令注册失败
// → initErr → Connect 返回错误，整机离线）。
func TestServiceAliasCollisionWithPlatformCommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c := New(Options{Key: "host_1.1.secret.owner", WorkDir: t.TempDir(), NoSandbox: true,
		MCP: mcpx.Settings{Servers: map[string]mcpx.Config{"ssh": {Command: "echo", NoSandbox: true}}}})
	c.hostID = "host_1"
	c.uid = "owner"
	c.kTool = "test-key"
	c.sessionRoot = t.TempDir()
	if c.initErr != nil {
		t.Fatalf("colliding service alias bricked init: %v", c.initErr)
	}
	if c.services.Has("ssh") {
		t.Fatal("colliding service must be skipped")
	}
	// 平台命令 ssh 仍是内建那个（usage 类退出码，而非 127 未注册）
	r := callTool(t, c, context.Background(), testCaller(), execRequest("ssh", 30000))
	out := decoded[protocol.Output](t, r.Result)
	if out.Attrs["exit_code"] == "127" {
		t.Fatalf("platform command shadowed by service: %+v", out)
	}
	// 被拒条目要在 mcp list 里可见（rejected），不能只留在 pod 日志：命令面
	// 没有它、但配置还在，用户看 list 就知道「配了却没生效」及其原因。
	r = callTool(t, c, context.Background(), testCaller(), execRequest("mcp list --json", 30000))
	out = decoded[protocol.Output](t, r.Result)
	var rows []struct{ Name, Kind, State, Error string }
	if err := json.Unmarshal([]byte(out.Content), &rows); err != nil {
		t.Fatalf("mcp list --json: %v (%q)", err, out.Content)
	}
	found := false
	for _, row := range rows {
		if row.Name == "ssh" {
			found = true
			if row.State != "rejected" || row.Error == "" {
				t.Fatalf("rejected alias must be surfaced: %+v", row)
			}
		}
	}
	if !found {
		t.Fatalf("rejected alias missing from mcp list: %s", out.Content)
	}
}

// registerService 换型判定：TCP↔MCP 互转必须摘除重登记（原实现漏判
// TCP→MCP，旧 TCP provider 钉旧地址继续服务）；同型 MCP/同址 TCP 不动。
func TestRegisterServiceTypeSwitch(t *testing.T) {
	c, _ := testClient(t)
	lookup := func() vtools.Provider {
		p, ok := c.services.Lookup("sw")
		if !ok {
			t.Fatal("service not registered")
		}
		return p
	}
	if err := c.registerService("sw", mcpx.Config{URL: "tcp://127.0.0.1:9866"}); err != nil {
		t.Fatal(err)
	}
	if _, isTCP := lookup().(*vtools.TCPProvider); !isTCP {
		t.Fatal("tcp config must register a TCPProvider")
	}
	// TCP → MCP：重登记为 MCP 适配器
	if err := c.registerService("sw", mcpx.Config{Command: "echo"}); err != nil {
		t.Fatal(err)
	}
	if _, isMCP := lookup().(*vtools.MCPProvider); !isMCP {
		t.Fatal("TCP→MCP switch must re-register as MCPProvider")
	}
	// MCP → TCP：重登记回 TCP
	if err := c.registerService("sw", mcpx.Config{URL: "tcp://127.0.0.1:9866"}); err != nil {
		t.Fatal(err)
	}
	if _, isTCP := lookup().(*vtools.TCPProvider); !isTCP {
		t.Fatal("MCP→TCP switch must re-register as TCPProvider")
	}

	// TCP 后端的包装 provider（blenderProvider）：地址按接口判定，不断言具体
	// 类型——否则它会被当成「非 TCP」，换成 MCP 配置时留在原地继续服务旧端点。
	wrapped := &blenderProvider{addr: "127.0.0.1:9866"}
	if err := c.services.Register("bl", wrapped); err != nil {
		t.Fatal(err)
	}
	blLookup := func() vtools.Provider {
		p, ok := c.services.Lookup("bl")
		if !ok {
			t.Fatal("wrapped service not registered")
		}
		return p
	}
	if err := c.registerService("bl", mcpx.Config{URL: "tcp://127.0.0.1:9866"}); err != nil {
		t.Fatal(err)
	}
	if blLookup() != vtools.Provider(wrapped) {
		t.Fatal("same TCP endpoint must keep the wrapping provider")
	}
	if err := c.registerService("bl", mcpx.Config{Command: "echo"}); err != nil {
		t.Fatal(err)
	}
	if _, isMCP := blLookup().(*vtools.MCPProvider); !isMCP {
		t.Fatal("TCP-backed wrapper → MCP config must re-register as MCPProvider")
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

// 配置里 disabled 的 TCP 条目也要在 mcp list 曝光（原既不 registered 也不
// rejected，「禁用了 TCP 服务」在任何清单都不可见）。
func TestMCPListShowsDisabledTCP(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c := New(Options{Key: "host_1.1.secret.owner", WorkDir: t.TempDir(), NoSandbox: true,
		MCP: mcpx.Settings{Servers: map[string]mcpx.Config{
			"off-tcp": {URL: "tcp://127.0.0.1:9999", Disabled: true},
		}}})
	c.hostID = "host_1"
	c.uid = "owner"
	c.kTool = "test-key"
	c.sessionRoot = t.TempDir()
	if c.initErr != nil {
		t.Fatal(c.initErr)
	}
	var out strings.Builder
	if err := c.mcpList(&commands.Invocation{Stdout: &out}, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "off-tcp\ttcp\tdisabled") {
		t.Fatalf("disabled tcp 未曝光:\n%s", out.String())
	}
}
