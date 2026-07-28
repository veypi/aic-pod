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

