package host

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"

	"github.com/veypi/aic-pod/libs/proto"
	"github.com/veypi/aic-pod/libs/vcore"
)

// 壳 provider 注册（§5.1）：声明进命令表 + caps，分发路由到 provider 执行体。
func TestProviderRegisterAndDispatch(t *testing.T) {
	decl, ok := vcore.Decl("browser")
	if !ok {
		t.Fatal("vcore browser decl missing")
	}
	var gotArgv []string
	var gotSid string
	err := RegisterProvider(Provider{
		Decl: decl,
		Run: func(ctx context.Context, sid string, req *proto.ToolRequest, argv []string) *proto.ToolResponse {
			gotSid = sid
			gotArgv = argv
			return &proto.ToolResponse{MsgID: req.MsgID, State: proto.StateCompleted,
				Content: "✓ ok", Attrs: map[string]string{"action": "browser"}}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		providersMu.Lock()
		delete(providers, "browser")
		providersMu.Unlock()
	})

	// New 之后注册的 provider 即时入表
	c, _ := testClient(t)
	c.addProvider(decl)

	// caps 声明包含 browser（desc/help/level 与 vcore 元数据同源）
	found := false
	for _, v := range c.buildCaps().Exec.Commands {
		if v.Name == "browser" {
			found = true
			if v.Desc == "" || v.Help == "" || v.RequiredLevel != 1 {
				t.Errorf("browser decl incomplete: %+v", v)
			}
		}
	}
	if !found {
		t.Fatal("caps missing browser command (provider registered)")
	}
	// commands 输出包含 browser
	if !strings.Contains(c.commandsJSON(), `"browser"`) {
		t.Fatal("commands output missing browser")
	}

	// 分发：exec browser 路由到 provider
	data := signedReq(t, c, "exec", map[string]any{
		"action": "browser", "argv": []string{"snapshot", "--interactive"},
	}, 3)
	resp := c.dispatch(context.Background(), testSubject, data)
	if resp.State != proto.StateCompleted || resp.Content != "✓ ok" {
		t.Fatalf("provider dispatch: %+v", resp)
	}
	if gotSid != "s1" {
		t.Errorf("sid = %q, want s1", gotSid)
	}
	if len(gotArgv) != 2 || gotArgv[0] != "snapshot" || gotArgv[1] != "--interactive" {
		t.Errorf("argv = %v", gotArgv)
	}

	// 纵深检查：provider 命令声明基线 Write(2)（动态表取高不取低，§6.2）：
	// snapshot granted=2 放行；click granted=1 → waiting
	data2 := signedReq(t, c, "exec", map[string]any{
		"action": "browser", "argv": []string{"snapshot"},
	}, 2)
	if resp2 := c.dispatch(context.Background(), testSubject, data2); resp2.State != proto.StateCompleted {
		t.Errorf("snapshot with granted=2 should pass (Write 基线): %+v", resp2)
	}
	// 写子命令（click）granted=1 → waiting
	data3 := signedReq(t, c, "exec", map[string]any{
		"action": "browser", "argv": []string{"click", "@s12:e1"},
	}, 1)
	if resp3 := c.dispatch(context.Background(), testSubject, data3); resp3.State != proto.StateRejected {
		t.Errorf("click with granted=1 should be waiting (Write): %+v", resp3)
	}
}

// 壳通道往返（RunViaShell）：本地 TCP stub 模拟壳进程应答。
func TestRunViaShell(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				line, err := bufio.NewReader(c).ReadString('\n')
				if err != nil {
					return
				}
				var req ShellRequest
				if json.Unmarshal([]byte(line), &req) != nil {
					return
				}
				if req.Token != "tok1" {
					c.Write([]byte(`{"state":"error","error":"bad token"}` + "\n"))
					return
				}
				out, _ := json.Marshal(ShellResponse{
					Content: "echo:" + strings.Join(req.Argv, " "),
					Attrs:   map[string]string{"sid": req.SessionID},
				})
				c.Write(append(out, '\n'))
			}(conn)
		}
	}()

	run := RunViaShell(ShellChannel{Addr: ln.Addr().String(), Token: "tok1"})
	resp := run(context.Background(), "s9", &proto.ToolRequest{MsgID: "m1", GrantedLevel: 9}, []string{"open", "https://x"})
	if resp.State != proto.StateCompleted || resp.Content != "echo:open https://x" {
		t.Fatalf("resp = %+v", resp)
	}
	if resp.Attrs["sid"] != "s9" {
		t.Errorf("attrs = %v", resp.Attrs)
	}

	// 通道不可达：干净错误（不 panic）
	run2 := RunViaShell(ShellChannel{Addr: "127.0.0.1:1", Token: "x"})
	resp2 := run2(context.Background(), "s9", &proto.ToolRequest{MsgID: "m2"}, nil)
	if resp2.State != proto.StateError || !strings.Contains(resp2.Error, "dial failed") {
		t.Errorf("unreachable channel: %+v", resp2)
	}
}

// 启动窗口期对账（syncProviders）：先建会话、后到达的 register 经对账补入命令表。
func TestSyncProvidersStartupReconcile(t *testing.T) {
	decl, ok := vcore.Decl("browser")
	if !ok {
		t.Fatal("vcore browser decl missing")
	}
	// 清干净后建会话：此刻进程注册表无 browser → 命令表不含 browser。
	providersMu.Lock()
	delete(providers, "browser")
	providersMu.Unlock()
	c, _ := testClient(t)
	if _, ok := c.cmdByName["browser"]; ok {
		t.Fatal("precondition: browser should be absent before register")
	}

	// 模拟壳注册在会话就绪前到达（rtClient 未赋值 → 只写进程级注册表）。
	if err := RegisterProvider(Provider{Decl: decl, Run: func(context.Context, string, *proto.ToolRequest, []string) *proto.ToolResponse {
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		providersMu.Lock()
		delete(providers, "browser")
		providersMu.Unlock()
	})

	// 对账：补入命令表（changed=true）；重复调用幂等（无变化）。
	if !c.syncProviders() {
		t.Fatal("syncProviders should report change")
	}
	if _, ok := c.cmdByName["browser"]; !ok {
		t.Fatal("browser missing from command table after reconcile")
	}
	if c.syncProviders() {
		t.Fatal("syncProviders should be idempotent (no change on second call)")
	}

	// caps 构造包含 browser（desc/level 与 vcore 元数据同源）。
	found := false
	for _, v := range c.buildCaps().Exec.Commands {
		if v.Name == "browser" {
			found = true
			if v.RequiredLevel != 1 {
				t.Errorf("browser decl level = %d, want 1", v.RequiredLevel)
			}
		}
	}
	if !found {
		t.Fatal("caps missing browser after reconcile")
	}
}
