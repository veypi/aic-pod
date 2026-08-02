package host

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/veypi/aic-pod/sdk/proto"
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

	// 合法签名 → 执行（ls 是虚拟指令，granted 1 足够）
	raw1 := signedReq(t, c, "exec", map[string]any{"action": "ls", "argv": []string{}}, 1)
	resp := c.dispatch(context.Background(), testSubject, raw1)
	if resp.State != proto.StateCompleted {
		t.Fatalf("state = %s err = %q", resp.State, resp.Error)
	}

	// 篡改签名 → rejected
	raw := signedReq(t, c, "exec", map[string]any{"action": "ls"}, 1)
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
	raw2 := signedReq(t, c, "exec", map[string]any{"action": "ls"}, 1)
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

	// granted=1 执行程序命令（基线 Danger 3）→ waiting 动态审批
	resp := c.dispatch(context.Background(), testSubject, signedReq(t, c, "exec",
		map[string]any{"action": "bash", "argv": []string{"-c", "true"}}, 1))
	if resp.State != proto.StateWaiting || resp.NeedApproval == nil {
		t.Fatalf("program low grant: state = %s needApproval = %v", resp.State, resp.NeedApproval)
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
	if len(cmds) < 12 { // 核心 8 + commands + bg_*3（探测项另计）
		t.Errorf("commands = %d", len(cmds))
	}
	hasRg, hasTree := false, false
	for _, v := range cmds {
		if decl, ok := v.(map[string]any); ok {
			switch decl["name"] {
			case "rg":
				hasRg = true
			case "tree":
				hasTree = true
			}
			if _, hasDesc := decl["desc"]; !hasDesc {
				t.Errorf("decl missing desc: %v", decl)
			}
			if _, hasLevel := decl["level"]; hasLevel {
				t.Errorf("level should not be exposed to AI: %v", decl)
			}
		}
	}
	if !hasRg {
		t.Error("command table missing rg")
	}
	if !hasTree {
		t.Error("command table missing tree")
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

	// 虚拟优先：ls 走虚拟指令而非本地命令（§5.4）
	resp = c.dispatch(context.Background(), testSubject, signedReq(t, c, "exec",
		map[string]any{"action": "ls", "argv": []string{}}, 1))
	if resp.State != proto.StateCompleted || resp.Attrs["action"] != "ls" {
		t.Fatalf("virtual ls: state=%s attrs=%v", resp.State, resp.Attrs)
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
		{"wss://ivec.ai/aic/api/nc", "wss://ivec.ai", "/aic/api/nc"},
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
	if !strings.Contains(string(log), " open https://example.com") {
		t.Errorf("browser args = %s", log)
	}
}
