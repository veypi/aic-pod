package host

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/veypi/aic-pod/libs/proto"
)

// dispatch 单测：验签 → deadline → nonce 去重 → granted 纵深检查 → fs/exec 分发。

func testClient(t *testing.T) (*Client, string) {
	t.Helper()
	c := New(Options{WorkDir: t.TempDir(), ExecTimeout: time.Minute})
	c.hostID = "host_test01"
	c.uid = "u1"
	c.credVer = 1
	_, _, kTool, err := proto.DeriveKeys("test-secret", c.hostID)
	if err != nil {
		t.Fatal(err)
	}
	c.kTool = kTool
	return c, kTool
}

func signedReq(t *testing.T, c *Client, tool string, data any, granted int) []byte {
	t.Helper()
	raw, _ := json.Marshal(data)
	nonce, _ := proto.NewNonce()
	req := &proto.ToolRequest{
		MsgID:        "msg_" + nonce[:6],
		SessionID:    "s1",
		Tool:         tool,
		Data:         raw,
		GrantedLevel: granted,
		Nonce:        nonce,
		Deadline:     time.Now().Add(time.Minute).UTC().Format(time.RFC3339),
	}
	proto.SignToolRequest(req, c.hostID, c.kTool)
	out, _ := json.Marshal(req)
	return out
}

const testSubject = "u.u1.s.s1.h.host_test01.exec.req"

func TestDispatchVerifyChain(t *testing.T) {
	c, _ := testClient(t)

	// 合法签名 → 执行（commands 是虚拟指令，granted 1 足够）
	raw1 := signedReq(t, c, "exec", map[string]any{"action": "commands", "argv": []string{}}, 1)
	resp := c.dispatch(context.Background(), testSubject, raw1)
	if resp.State != proto.StateCompleted {
		t.Fatalf("state = %s err = %q", resp.State, resp.Error)
	}

	// 篡改签名 → rejected
	raw := signedReq(t, c, "exec", map[string]any{"action": "commands"}, 1)
	var req proto.ToolRequest
	_ = json.Unmarshal(raw, &req)
	req.GrantedLevel = 9 // 篡改 granted_level
	tampered, _ := json.Marshal(&req)
	resp = c.dispatch(context.Background(), testSubject, tampered)
	if resp.State != proto.StateRejected || resp.Error != "invalid request signature" {
		t.Fatalf("tampered: state = %s err = %q", resp.State, resp.Error)
	}

	// 同一 nonce 重放 → rejected duplicate nonce
	resp = c.dispatch(context.Background(), testSubject, raw1)
	if resp.State != proto.StateRejected || resp.Error != "duplicate nonce" {
		t.Fatalf("replay: state = %s err = %q", resp.State, resp.Error)
	}

	// 过期 deadline → rejected request expired
	raw2 := signedReq(t, c, "exec", map[string]any{"action": "commands"}, 1)
	_ = json.Unmarshal(raw2, &req)
	req.Deadline = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	proto.SignToolRequest(&req, c.hostID, c.kTool)
	expired, _ := json.Marshal(&req)
	resp = c.dispatch(context.Background(), testSubject, expired)
	if resp.State != proto.StateRejected || resp.Error != "request expired" {
		t.Fatalf("expired: state = %s err = %q", resp.State, resp.Error)
	}
}

func TestDispatchGrantedDepthCheck(t *testing.T) {
	c, _ := testClient(t)
	// 该用例验证授权深度检查（granted 与 required 的匹配），与沙箱无关：
	// 全局免沙箱隔离环境差异（§5.10），使「放行」分支聚焦授权语义。
	c.procs.NoSandbox = true

	// granted=0 显式禁用 → rejected（不可审批，§2.4 level 0 语义）
	resp := c.dispatch(context.Background(), testSubject, signedReq(t, c, "exec",
		map[string]any{"action": "commands", "argv": []string{}}, 0))
	if resp.State != proto.StateRejected || resp.NeedApproval != nil {
		t.Fatalf("level 0: state = %s needApproval = %v err = %q", resp.State, resp.NeedApproval, resp.Error)
	}

	// granted=1 执行程序命令（基线 Danger 3）→ waiting 动态审批
	resp = c.dispatch(context.Background(), testSubject, signedReq(t, c, "exec",
		map[string]any{"action": "bash", "argv": []string{"-c", "true"}}, 1))
	if resp.State != proto.StateWaiting || resp.NeedApproval == nil {
		t.Fatalf("program low grant: state = %s needApproval = %v", resp.State, resp.NeedApproval)
	}

	// nosandbox → required 提升 Critical(4)：granted 3（用户可授予上限）仍 waiting（必审批）
	resp = c.dispatch(context.Background(), testSubject, signedReq(t, c, "exec",
		map[string]any{"action": "bash", "argv": []string{"-c", "true"}, "nosandbox": true}, 3))
	if resp.State != proto.StateWaiting || resp.NeedApproval == nil {
		t.Fatalf("nosandbox grant 3: state = %s needApproval = %v", resp.State, resp.NeedApproval)
	}

	// granted=3 执行程序命令 → 放行（true 立即返回）
	resp = c.dispatch(context.Background(), testSubject, signedReq(t, c, "exec",
		map[string]any{"action": "bash", "argv": []string{"-c", "echo hi"}}, 3))
	if resp.State != proto.StateCompleted {
		t.Fatalf("program ok: state = %s err = %q", resp.State, resp.Error)
	}

	// fs write granted=1 < required=2 → waiting
	resp = c.dispatch(context.Background(), "u.u1.s.s1.h.host_test01.fs.req", signedReq(t, c, "fs",
		map[string]any{"action": "write", "path": "a.txt", "content": "x"}, 1))
	if resp.State != proto.StateWaiting {
		t.Fatalf("fs write low grant: state = %s", resp.State)
	}

	// fs write granted=2 → 执行（workdir 为 host 配置工作区）
	resp = c.dispatch(context.Background(), "u.u1.s.s1.h.host_test01.fs.req", signedReq(t, c, "fs",
		map[string]any{"action": "write", "path": "a.txt", "content": "x"}, 2))
	if resp.State != proto.StateCompleted {
		t.Fatalf("fs write ok: state = %s err = %q", resp.State, resp.Error)
	}
}

func TestDispatchVirtualCommands(t *testing.T) {
	c, _ := testClient(t)

	// commands：返回统一命令表（{name, desc} 视图，无 fs/programs 字段）
	resp := c.dispatch(context.Background(), testSubject, signedReq(t, c, "exec",
		map[string]any{"action": "commands"}, 1))
	if resp.State != proto.StateCompleted {
		t.Fatalf("commands: %s %q", resp.State, resp.Error)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(resp.Content), &payload); err != nil {
		t.Fatal(err)
	}
	if _, ok := payload["fs"]; ok {
		t.Errorf("commands reply should not contain fs field: %v", payload)
	}
	cmds, ok := payload["commands"].([]any)
	if !ok {
		t.Fatalf("commands payload = %v", payload)
	}
	if len(cmds) < 6 { // curl + json + commands + bg_*3（探测项另计）
		t.Errorf("commands = %d", len(cmds))
	}
	hasCurl, hasJSON := false, false
	for _, v := range cmds {
		if decl, ok := v.(map[string]any); ok {
			switch decl["name"] {
			case "curl":
				hasCurl = true
			case "json":
				hasJSON = true
			case "ls", "rg", "tree", "rm", "mkdir", "cp", "mv":
				t.Errorf("file command should not be declared by exec: %v", decl["name"])
			}
			if _, hasDesc := decl["desc"]; !hasDesc {
				t.Errorf("decl missing desc: %v", decl)
			}
			if _, hasLevel := decl["level"]; hasLevel {
				t.Errorf("level should not be exposed to AI: %v", decl)
			}
		}
	}
	if !hasCurl {
		t.Error("command table missing curl")
	}
	if !hasJSON {
		t.Error("command table missing json")
	}

	// 未声明命令 → error（统一命令声明模型：无透传）
	resp = c.dispatch(context.Background(), testSubject, signedReq(t, c, "exec",
		map[string]any{"action": "nonexistent-cmd-xyz"}, 3))
	if resp.State != proto.StateError {
		t.Fatalf("undeclared command: state = %s", resp.State)
	}
	if !strings.Contains(resp.Error, "not declared by this host") {
		t.Errorf("undeclared error = %q", resp.Error)
	}

	// 文件类指令已迁入 fs：exec 不再声明 ls（§4/§5 拆分；granted=3 越过纵深检查达路由）
	resp = c.dispatch(context.Background(), testSubject, signedReq(t, c, "exec",
		map[string]any{"action": "ls", "argv": []string{}}, 3))
	if resp.State != proto.StateError || !strings.Contains(resp.Error, "not declared by this host") {
		t.Fatalf("ls should be undeclared on exec: state=%s err=%q", resp.State, resp.Error)
	}

	// 虚拟指令分发：curl 缺参数报参数错误（证明路由到 vcore 而非未声明）
	resp = c.dispatch(context.Background(), testSubject, signedReq(t, c, "exec",
		map[string]any{"action": "curl", "argv": []string{}}, 2))
	if resp.State != proto.StateError || !strings.Contains(resp.Error, "missing argument") {
		t.Fatalf("virtual curl: state=%s err=%q", resp.State, resp.Error)
	}
}

func TestDispatchBgFlow(t *testing.T) {
	c, _ := testClient(t)

	// bg_list 空 → rows 0
	resp := c.dispatch(context.Background(), testSubject, signedReq(t, c, "exec",
		map[string]any{"action": "bg_list"}, 1))
	if resp.State != proto.StateCompleted || resp.Attrs["rows"] != "0" {
		t.Fatalf("bg_list: %v", resp.Attrs)
	}

	// bg_wait 不存在的 id → error
	resp = c.dispatch(context.Background(), testSubject, signedReq(t, c, "exec",
		map[string]any{"action": "bg_wait", "argv": []string{"host_test01:s1:nope"}}, 1))
	if resp.State != proto.StateError {
		t.Fatalf("bg_wait missing: state = %s", resp.State)
	}
}

// 空 nonce / 非法 deadline 必须拒绝（§6.2 纵深防御：防跳过去重、防"永不过期"请求）。
func TestDispatchRejectsWeakEnvelope(t *testing.T) {
	c, kTool := testClient(t)
	newReq := func(nonce, deadline string) []byte {
		raw, _ := json.Marshal(map[string]any{"action": "commands"})
		req := &proto.ToolRequest{
			MsgID: "msg_weak", SessionID: "s1", Tool: "exec", Data: raw,
			GrantedLevel: 3, Nonce: nonce, Deadline: deadline,
		}
		proto.SignToolRequest(req, c.hostID, kTool)
		out, _ := json.Marshal(req)
		return out
	}
	valid := time.Now().Add(time.Minute).UTC().Format(time.RFC3339)

	if resp := c.dispatch(context.Background(), testSubject, newReq("", valid)); resp.State != proto.StateRejected {
		t.Errorf("empty nonce: state = %s, want rejected", resp.State)
	}
	nonce, _ := proto.NewNonce()
	if resp := c.dispatch(context.Background(), testSubject, newReq(nonce, "not-a-timestamp")); resp.State != proto.StateRejected {
		t.Errorf("invalid deadline: state = %s, want rejected", resp.State)
	}
	if resp := c.dispatch(context.Background(), testSubject, newReq(nonce, "")); resp.State != proto.StateRejected {
		t.Errorf("missing deadline: state = %s, want rejected", resp.State)
	}
}

// parseWSURL 的 base/proxyPath 切分须按实际 scheme 长度跳过 "://"。
// 回归：wss:// 曾被按 ws:// 的起点扫描，第二个斜杠被误判为路径起点。
func TestParseWSURL(t *testing.T) {
	cases := []struct {
		raw       string
		base      string
		proxyPath string
	}{
		{"wss://ivec.ai/api/nc", "wss://ivec.ai", "/api/nc"},
		{"ws://host/path", "ws://host", "/path"},
		{"wss://host", "wss://host", ""},
		{"ws://host:8080/a/b", "ws://host:8080", "/a/b"},
		{"ws://host/", "ws://host/", ""},
	}
	for _, c := range cases {
		u, err := parseWSURL(c.raw)
		if err != nil {
			t.Errorf("%s: unexpected err %v", c.raw, err)
			continue
		}
		if u.base != c.base || u.proxyPath != c.proxyPath {
			t.Errorf("%s: got base=%q proxy=%q, want base=%q proxy=%q",
				c.raw, u.base, u.proxyPath, c.base, c.proxyPath)
		}
	}
}

// 物理 host 集成 browser（§5.6 pod 模式）：探测声明 + 分发走 vcore/browser。
func TestDispatchBrowserVirtual(t *testing.T) {
	// stub agent-browser 先入 PATH（探测发生在 New），再建客户端
	dir := t.TempDir()
	logPath := filepath.Join(dir, "cli.log")
	script := "#!/bin/sh\necho \"$@\" >> " + logPath + "\necho ok\n"
	if err := os.WriteFile(filepath.Join(dir, "agent-browser"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	c, _ := testClient(t)
	c.opts.WorkDir = dir

	// caps 声明包含 browser（desc/help/level 与 vcore 元数据同源）
	caps := c.buildCaps()
	found := false
	for _, v := range caps.Exec.Commands {
		if v.Name == "browser" {
			found = true
			if v.Desc == "" || v.Help == "" || v.RequiredLevel == 0 {
				t.Errorf("browser decl incomplete: %+v", v)
			}
		}
	}
	if !found {
		t.Fatal("caps missing browser command (agent-browser probed)")
	}

	// 验证分发走 vcore/browser 且参数不带 --session/--namespace（pod 不隔离）
	data := signedReq(t, c, "exec", map[string]any{
		"action": "browser", "argv": []string{"open", "https://example.com"},
	}, 3)
	resp := c.dispatch(context.Background(), testSubject, data)
	if resp.State != proto.StateCompleted {
		t.Fatalf("browser dispatch: %+v", resp)
	}
	log, _ := os.ReadFile(logPath)
	if strings.Contains(string(log), "--session") || strings.Contains(string(log), "--namespace") {
		t.Errorf("pod browser should not pass --session/--namespace: %s", log)
	}
	if !strings.Contains(string(log), "open https://example.com") {
		t.Errorf("browser args = %s", log)
	}
}

// 本地命令 workdir 缺省回落：请求未携带 workdir 时使用 host 端配置工作区（§2.1.1）。
func TestExecCmdWorkdirFallback(t *testing.T) {
	wd := t.TempDir()
	// 该用例验证 workdir 缺省回落机制，与沙箱无关：全局免沙箱隔离环境差异（§5.10）
	c := New(Options{WorkDir: wd, ExecTimeout: time.Minute, NoSandbox: true})
	c.hostID = "host_test01"

	// 不带 workdir → bash -c pwd 应返回配置工作区
	data := signedReq(t, c, "exec", map[string]any{
		"action": "bash", "argv": []string{"-c", "pwd"},
	}, 9)
	resp := c.dispatch(context.Background(), testSubject, data)
	if resp.State != proto.StateCompleted {
		t.Fatalf("dispatch: %+v", resp)
	}
	if !strings.Contains(resp.Content, wd) {
		t.Fatalf("cwd = %q, want contains %q", resp.Content, wd)
	}

	// 显式 workdir 优先
	data2 := signedReq(t, c, "exec", map[string]any{
		"action": "bash", "argv": []string{"-c", "pwd"}, "workdir": "/tmp",
	}, 9)
	resp2 := c.dispatch(context.Background(), testSubject, data2)
	if resp2.State != proto.StateCompleted {
		t.Fatalf("dispatch: %+v", resp2)
	}
	if !strings.Contains(resp2.Content, "/tmp") || strings.Contains(resp2.Content, wd) {
		t.Fatalf("explicit workdir not honored: %q (want /tmp)", resp2.Content)
	}
}
