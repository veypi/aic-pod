package host

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/protocol"
	"github.com/veypi/vsh/commands"
)

// fakeBlenderAddon 是 vtools/tcp v1 的假 addon：tools 回两条目录，ping 回握手，
// echo 回显 arguments（覆盖 flag 映射经自定义命令透传到 Provider 的链路）。
// 返回关闭函数：断 listener 与全部已接受连接（模拟 Blender 退出）。
func fakeBlenderAddon(t *testing.T) (addr string, closeAll func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	closed := false
	closeAll = func() {
		mu.Lock()
		defer mu.Unlock()
		closed = true
		_ = listener.Close()
		for _, c := range conns {
			_ = c.Close()
		}
	}
	t.Cleanup(closeAll)
	tools := []map[string]any{
		{"name": "ping", "description": "握手", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}},
		{"name": "echo", "description": "回显", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}}},
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			if closed {
				mu.Unlock()
				_ = conn.Close()
				return
			}
			conns = append(conns, conn)
			mu.Unlock()
			go func() {
				defer conn.Close()
				scanner := bufio.NewScanner(conn)
				scanner.Buffer(make([]byte, 64<<10), 16<<20)
				for scanner.Scan() {
					var req struct {
						ID        int            `json:"id"`
						Method    string         `json:"method"`
						Name      string         `json:"name"`
						Arguments map[string]any `json:"arguments"`
					}
					if json.Unmarshal(scanner.Bytes(), &req) != nil {
						continue
					}
					rep := map[string]any{"id": req.ID}
					switch {
					case req.Method == "tools":
						rep["tools"] = tools
					case req.Method == "call" && req.Name == "ping":
						rep["result"] = map[string]any{"ok": true, "addon": "0.1.0", "protocol": "vtools/tcp v1", "blender": "4.2.0-test"}
					case req.Method == "call" && req.Name == "echo":
						rep["result"] = map[string]any{"echo": req.Arguments}
					default:
						rep["error"] = "unknown"
					}
					raw, _ := json.Marshal(rep)
					_, _ = conn.Write(append(raw, '\n'))
				}
			}()
		}
	}()
	return listener.Addr().String(), closeAll
}

// doctor 分级：L1 未安装（含写错的 env 覆盖）→ L2 没在跑 → L3 kind=ok。
func TestBlenderDoctorLevels(t *testing.T) {
	c, _ := testClient(t)
	run := func(args ...string) (string, int) {
		engine, err := c.engine()
		if err != nil {
			t.Fatal(err)
		}
		cmd, ok := engine.Registry().Lookup("blender")
		if !ok {
			t.Fatal("blender command not registered")
		}
		var out, errBuf strings.Builder
		err = cmd.Run(context.Background(), &commands.Invocation{Args: args, Stdout: &out, Stderr: &errBuf})
		code, _ := commands.ExitCode(err)
		return out.String(), code
	}

	// L1：env 覆盖指向不存在的路径
	t.Setenv("AIC_BLENDER_EXE", filepath.Join(t.TempDir(), "ghost-blender"))
	text, code := run("doctor")
	if code != 1 || !strings.Contains(text, "blender_not_installed") || !strings.Contains(text, "brew install") {
		t.Fatalf("L1: code=%d\n%s", code, text)
	}
	// L2：exe 存在但端口不通
	exe := filepath.Join(t.TempDir(), "blender")
	if err := os.WriteFile(exe, []byte("#!"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AIC_BLENDER_EXE", exe)
	t.Setenv("AIC_BLENDER_PORT", "1") // 1 端口必不通
	text, code = run("doctor")
	if code != 1 || !strings.Contains(text, "blender_not_running") || !strings.Contains(text, "blender launch") {
		t.Fatalf("L2: code=%d\n%s", code, text)
	}
	// L3：假 addon 握手 → kind=ok
	addr, _ := fakeBlenderAddon(t)
	_, port, _ := net.SplitHostPort(addr)
	t.Setenv("AIC_BLENDER_PORT", port)
	text, code = run("doctor")
	if code != 0 || !strings.Contains(text, "kind=ok") || !strings.Contains(text, "4.2.0-test") {
		t.Fatalf("L3: code=%d\n%s", code, text)
	}
}

// 自定义命令全链：vsh exec → 动词/工具透传 → 假 addon；门与目录/帮助。
func TestBlenderCommandEndToEnd(t *testing.T) {
	saved := cfg.Global
	cfg.Global = cfg.NewOptions()
	cfg.Global.ExecPolicy, cfg.Global.NetPolicy = cfg.PolicyOpen, cfg.PolicyOpen
	t.Cleanup(func() { cfg.Global = saved })
	addonAddr, closeAddon := fakeBlenderAddon(t)
	_, port, _ := net.SplitHostPort(addonAddr)
	t.Setenv("AIC_BLENDER_PORT", port)
	c, _ := testClient(t)
	run := func(script string) protocol.Output {
		t.Helper()
		r := callTool(t, c, context.Background(), testCaller(), execRequest(script, 30000))
		return decoded[protocol.Output](t, r.Result)
	}

	out := run("blender ping --json")
	if out.Attrs["exit_code"] != "0" || !strings.Contains(out.Content, `"4.2.0-test"`) {
		t.Fatalf("ping: %+v", out)
	}
	// flag 映射经自定义命令透传（--text → arguments.text）
	out = run("blender echo --text hello --json")
	if out.Attrs["exit_code"] != "0" || !strings.Contains(out.Content, `"hello"`) {
		t.Fatalf("echo: %+v", out)
	}
	// 目录与帮助
	if out = run("blender"); !strings.Contains(out.Content, "ping") || !strings.Contains(out.Content, "echo") {
		t.Fatalf("catalog: %q", out.Content)
	}
	if out = run("blender --help"); !strings.Contains(out.Content, "blender doctor") {
		t.Fatalf("help: %q", out.Content)
	}
	// 发现面含 blender
	if out = run("commands"); !strings.Contains(out.Content, "blender") {
		t.Fatalf("discoverable: %q", out.Content)
	}
	// 未运行时的分级错误（关掉假 addon 再调）
	closeAddon()
	out = run("blender ping --json")
	if out.Attrs["exit_code"] != "1" || !strings.Contains(out.Attrs["stderr"]+out.Content, "blender_not_running") {
		t.Fatalf("not-running translation: %+v", out)
	}
}

// blender 的门：deny 策略下工具/launch 126；doctor 是只读探测，免门。
func TestBlenderGate(t *testing.T) {
	saved := cfg.Global
	cfg.Global = cfg.NewOptions()
	cfg.Global.ExecPolicy = cfg.PolicyDeny
	t.Cleanup(func() { cfg.Global = saved })
	t.Setenv("AIC_BLENDER_EXE", filepath.Join(t.TempDir(), "ghost"))
	c, _ := testClient(t)
	run := func(script string) protocol.Output {
		t.Helper()
		r := callTool(t, c, context.Background(), testCaller(), execRequest(script, 30000))
		return decoded[protocol.Output](t, r.Result)
	}
	if out := run("blender ping"); out.Attrs["exit_code"] != "126" || !strings.Contains(out.Attrs["stderr"], "mcp.blender") {
		t.Fatalf("tool gate: %+v", out)
	}
	if out := run("blender launch"); out.Attrs["exit_code"] != "126" {
		t.Fatalf("launch gate: %+v", out)
	}
	// doctor 免门：落到 L1 分级错误（exit 1），不是 126
	if out := run("blender doctor"); out.Attrs["exit_code"] != "1" || !strings.Contains(out.Content, "blender_not_installed") {
		t.Fatalf("doctor must be gate-exempt: %+v", out)
	}
}

// 内嵌包登记失败只记日志跳过，不拖垮整机（回归：原 registerBuiltinPacks
// return err → initErr → Connect 失败，坏包/撞名会让 pod 整机离线）。
func TestRegisterPackServiceFailureIsolated(t *testing.T) {
	c, _ := testClient(t)
	bad := &packManifest{Alias: "bogus", Service: packService{Kind: "bogus"}}
	if err := c.registerPackService("bogus", bad); err == nil {
		t.Fatal("unknown service kind accepted")
	}
	badAddr := &packManifest{Alias: "badaddr", Service: packService{Kind: "tcp", URL: "tcp://bad addr"}}
	if err := c.registerPackService("badaddr", badAddr); err == nil {
		t.Fatal("invalid tcp addr accepted")
	}
	if c.initErr != nil {
		t.Fatalf("坏包污染 initErr: %v", c.initErr)
	}
	if c.services.Has("bogus") || c.services.Has("badaddr") {
		t.Fatal("登记失败的包留下了服务")
	}
}

// blenderProvider 地址在构造时钉死：之后改 AIC_BLENDER_PORT 不影响已登记
// provider（Addr() 与真实拨号同源，重启 pod 才生效）。
func TestBlenderProviderAddrPinned(t *testing.T) {
	m := &packManifest{
		Service:   packService{URL: "tcp://127.0.0.1:9866"},
		Lifecycle: &packLifecycle{PortEnv: "AIC_BLENDER_PORT_TEST"},
	}
	t.Setenv("AIC_BLENDER_PORT_TEST", "10001")
	p := newBlenderProvider(m)
	t.Setenv("AIC_BLENDER_PORT_TEST", "10002")
	if p.Addr() != "127.0.0.1:10001" {
		t.Fatalf("addr 未在构造时钉死: %s", p.Addr())
	}
}
