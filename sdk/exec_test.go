package aichost

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExecBasic(t *testing.T) {
	ctx := context.Background()
	r, err := handleExec(ctx, &ToolParams{Action: "echo", Argv: []string{"hello"}}, t.TempDir(), "s1", "m1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != "" {
		t.Fatalf("error = %q", r.Error)
	}
	if r.Content != "1\thello" {
		t.Fatalf("content = %q", r.Content)
	}
	if r.Attrs["action"] != "echo" || r.Attrs["rows"] != "1" || r.Attrs["path"] == "" {
		t.Fatalf("attrs = %v", r.Attrs)
	}
	// 日志文件可用 fs read 读取
	if _, err := os.Stat(r.Attrs["path"]); err != nil {
		t.Fatalf("log file missing: %v", err)
	}
}

func TestExecUnknownAction(t *testing.T) {
	r, _ := handleExec(context.Background(), &ToolParams{Action: "no-such-program-xyz", Argv: []string{}}, t.TempDir(), "s1", "m2", time.Minute)
	if r.Error != `exec: unknown action "no-such-program-xyz"` {
		t.Fatalf("error = %q", r.Error)
	}
}

func TestExecNonZeroExit(t *testing.T) {
	r, _ := handleExec(context.Background(), &ToolParams{Action: "sh", Argv: []string{"-c", "echo oops; exit 3"}}, t.TempDir(), "s1", "m3", time.Minute)
	if !strings.Contains(r.Error, "exit status 3 (exit=3)") {
		t.Fatalf("error = %q", r.Error)
	}
	if !strings.Contains(r.Content, "1\toops") {
		t.Fatalf("content = %q", r.Content)
	}
}

func TestExecBackgroundLifecycle(t *testing.T) {
	// 服务端 deadline 极短 → 自动后台化
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	r, _ := handleExec(ctx, &ToolParams{Action: "sh", Argv: []string{"-c", "echo start; sleep 2; echo end"}}, t.TempDir(), "sbg", "m4", 10*time.Second)
	if r.Attrs["background"] != "true" {
		t.Fatalf("attrs = %v", r.Attrs)
	}
	logPath := r.Attrs["path"]

	// bg_list 可见
	r, _ = handleBgList("sbg")
	if r.Error != "" {
		t.Fatal(r.Error)
	}
	var items []map[string]any
	if err := json.Unmarshal([]byte(r.Content), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0]["log_path"] != logPath {
		t.Fatalf("bg_list = %s", r.Content)
	}
	if r.Attrs["rows"] != "1" || r.Attrs["action"] != "bg_list" {
		t.Fatalf("attrs = %v", r.Attrs)
	}

	// bg_wait 等结束 → exit_code=0
	r, _ = handleBgWait(&ToolParams{Argv: []string{logPath, "--wait", "5"}})
	if r.Error != "" {
		t.Fatal(r.Error)
	}
	if r.Attrs["exit_code"] != "0" || r.Attrs["background"] == "true" {
		t.Fatalf("attrs = %v", r.Attrs)
	}
	if !strings.Contains(r.Content, "end") {
		t.Fatalf("content = %q", r.Content)
	}

	// bg_kill 已终结条目 → 报退出码并清理
	r, _ = handleBgKill(&ToolParams{Argv: []string{logPath}})
	if !strings.Contains(r.Content, "already exited") {
		t.Fatalf("content = %q", r.Content)
	}

	// 再次 bg_wait → 不在注册表 → 统一错误
	r, _ = handleBgWait(&ToolParams{Argv: []string{logPath}})
	if r.Error != "exec bg_wait: no such background process: "+logPath {
		t.Fatalf("error = %q", r.Error)
	}
	// 但日志仍可用 fs read 查看
	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("log file should persist: %v", err)
	}
}

func TestExecBgKillRunning(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	r, _ := handleExec(ctx, &ToolParams{Action: "sh", Argv: []string{"-c", "sleep 60"}}, t.TempDir(), "skill", "m5", time.Minute)
	logPath := r.Attrs["path"]
	if r.Attrs["background"] != "true" {
		t.Fatalf("attrs = %v", r.Attrs)
	}

	r, _ = handleBgKill(&ToolParams{Argv: []string{logPath}})
	if r.Error != "" {
		t.Fatal(r.Error)
	}
	if !strings.Contains(r.Content, "killed background process: "+logPath) {
		t.Fatalf("content = %q", r.Content)
	}

	// 进程应已终结：再次 bg_wait 报统一错误
	r, _ = handleBgWait(&ToolParams{Argv: []string{logPath, "--wait", "1"}})
	if r.Error != "exec bg_wait: no such background process: "+logPath {
		t.Fatalf("error = %q", r.Error)
	}
}

func TestReplayCache(t *testing.T) {
	var c replayCache
	dl := time.Now().Add(time.Minute)
	if !c.checkAndMark("n1", dl) {
		t.Fatal("first use should pass")
	}
	if c.checkAndMark("n1", dl) {
		t.Fatal("duplicate nonce should be rejected")
	}
	if !c.checkAndMark("n2", dl) {
		t.Fatal("different nonce should pass")
	}
	// 过期条目淘汰后可复用
	var c2 replayCache
	if !c2.checkAndMark("n3", time.Now().Add(-time.Second)) {
		t.Fatal("first use should pass")
	}
	if !c2.checkAndMark("n3", time.Now().Add(-time.Second)) {
		t.Fatal("expired entry should be evicted")
	}
}

func TestVerifyApprovalFingerprint(t *testing.T) {
	// 与 aic types.BuildApprovalFingerprint 同公式：
	// fp:{sessionID}:{tool}:{policyVersion}:{sha256(jcs({target,action,argv}))[:16]}
	params := &ToolParams{Action: "write", Argv: []string{"/a.txt", "--content", "hi"}}
	hash := approvalInputHash("host_abc", params.Action, params.Argv)
	fp := "fp:sess1:fs:1:" + hash
	if !verifyApprovalFingerprint(fp, "sess1", "fs", "host_abc", params) {
		t.Fatal("valid fingerprint should pass")
	}
	// 改参数 → 不符
	if verifyApprovalFingerprint(fp, "sess1", "fs", "host_abc", &ToolParams{Action: "write", Argv: []string{"/b.txt"}}) {
		t.Fatal("different argv should fail")
	}
	// 不同 target → 不符
	if verifyApprovalFingerprint(fp, "sess1", "fs", "host_xyz", params) {
		t.Fatal("different target should fail")
	}
	// session/tool 段不符
	if verifyApprovalFingerprint(fp, "sess2", "fs", "host_abc", params) {
		t.Fatal("different session should fail")
	}
	// 格式错误
	if verifyApprovalFingerprint("bad-fp", "sess1", "fs", "host_abc", params) {
		t.Fatal("malformed fingerprint should fail")
	}
}

func TestApprovalInputHashMatchesJCS(t *testing.T) {
	// 规范化形式确定性：{"action":"read","argv":["/a","--limit","20"],"target":"cloud"}
	h := approvalInputHash("cloud", "read", []string{"/a", "--limit", "20"})
	if len(h) != 16 {
		t.Fatalf("hash len = %d", len(h))
	}
	for _, c := range h {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			t.Fatalf("not hex: %q", h)
		}
	}
	// target 为空时省略 target 键
	h1 := approvalInputHash("", "read", []string{"/a"})
	h2 := approvalInputHash("cloud", "read", []string{"/a"})
	if h1 == h2 {
		t.Fatal("empty vs non-empty target must differ")
	}
}

func TestActionRequiredLevel(t *testing.T) {
	def := ToolDef{
		RequiredLevel: 3,
		Actions:       []any{map[string]any{"name": "rm", "required_level": float64(2)}, "ls"},
	}
	if lv := actionRequiredLevel(def, "rm"); lv != 2 {
		t.Fatalf("rm level = %d", lv)
	}
	if lv := actionRequiredLevel(def, "ls"); lv != 3 {
		t.Fatalf("ls level = %d", lv)
	}
	if lv := actionRequiredLevel(def, "bash"); lv != 3 {
		t.Fatalf("undeclared level = %d", lv)
	}
}

// 确保临时日志目录隔离：日志按 {tmp}/aic/{session_id}/{msg_id}.log 组织
func TestExecLogPathLayout(t *testing.T) {
	r, _ := handleExec(context.Background(), &ToolParams{Action: "true", Argv: []string{}}, t.TempDir(), "sess-x", "msg-y", time.Minute)
	want := filepath.Join(os.TempDir(), "aic", "sess-x", "msg-y.log")
	if r.Attrs["path"] != want {
		t.Fatalf("path = %q, want %q", r.Attrs["path"], want)
	}
}

// TestApprovalInputHashPinnedVectors 跨实现固定向量：
// 与 JS browser/src/sdk/auth.js approvalInputHash 及 aic types.ApprovalInputHash 必须一致。
func TestApprovalInputHashPinnedVectors(t *testing.T) {
	cases := []struct {
		target, action string
		argv           []string
		want           string
	}{
		{"cloud", "read", []string{"/a", "--limit", "20"}, "5ff51decf470e46a"},
		{"host_abc", "write", []string{"/a.txt", "--content", "hi"}, "aec0feb022efb7b1"},
		{"host_abc", "rm", []string{"/tmp/x"}, "7798623f326a4ad3"},
	}
	for _, c := range cases {
		if got := approvalInputHash(c.target, c.action, c.argv); got != c.want {
			t.Errorf("approvalInputHash(%q, %q, %v) = %q, want %q", c.target, c.action, c.argv, got, c.want)
		}
	}
}
