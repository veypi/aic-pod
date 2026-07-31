package host

import (
	"context"
	"encoding/json"
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

	// commands：返回 caps v2 能力
	resp := c.dispatch(context.Background(), testSubject, signedReq(t, c, "exec",
		map[string]any{"action": "commands"}, 1))
	if resp.State != proto.StateCompleted {
		t.Fatalf("commands: %s %q", resp.State, resp.Error)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(resp.Content), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["programs"] != nil {
		t.Errorf("programs should be null (unrestricted), got %v", payload["programs"])
	}
	virtual := payload["virtual"].([]any)
	if len(virtual) < 12 { // 核心 8 + commands + bg_*3
		t.Errorf("virtual = %d", len(virtual))
	}

	// 未知程序 → error unknown action
	resp = c.dispatch(context.Background(), testSubject, signedReq(t, c, "exec",
		map[string]any{"action": "nonexistent-cmd-xyz"}, 3))
	if resp.State != proto.StateError {
		t.Fatalf("unknown program: state = %s", resp.State)
	}

	// 虚拟优先：ls 走虚拟指令而非程序（§5.4）
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
