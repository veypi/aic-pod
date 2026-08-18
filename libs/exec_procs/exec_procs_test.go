package exec_procs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStartNormalCompletion(t *testing.T) {
	m := NewManager(0)
	// 该用例验证正常完成机制（落盘/行数/内容），与沙箱无关：全局免沙箱隔离环境差异（§5.10）
	m.NoSandbox = true
	dir := t.TempDir()
	logPath := filepath.Join(dir, "out.log")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := m.Start(ctx, StartOptions{
		ID:      "h1:s1:op1",
		Command: "printf",
		LogPath: logPath,
		Exec:    []string{"bash", "-c", "printf 'a\\nb\\nc\\n'"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Background || res.Content != "a\nb\nc\n" || res.Lines != 3 {
		t.Errorf("res = %+v", res)
	}
	if data, _ := os.ReadFile(logPath); string(data) != "a\nb\nc\n" {
		t.Errorf("log = %q", data)
	}
}

func TestStartTimeoutBackground(t *testing.T) {
	m := NewManager(0)
	// 该用例验证超时转后台机制，与沙箱无关：全局免沙箱隔离环境差异（§5.10）
	m.NoSandbox = true
	dir := t.TempDir()
	logPath := filepath.Join(dir, "out.log")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	res, err := m.Start(ctx, StartOptions{
		ID:      "h1:s1:op2",
		Command: "sleep",
		LogPath: logPath,
		Exec:    []string{"bash", "-c", "printf 'partial\\n'; sleep 5; printf 'late\\n'"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Background || res.ID != "h1:s1:op2" {
		t.Fatalf("res = %+v", res)
	}
	if res.Content != "partial\n" {
		t.Errorf("partial content = %q", res.Content)
	}
	// bg_list 应包含该条目
	if got := len(m.List()); got != 1 {
		t.Fatalf("bg_list = %d, want 1", got)
	}
	// bg_wait 等待完成
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer waitCancel()
	// 用短 wait 轮询直到完成（测试进程 sleep 5s 太长——直接 kill）
	if err := m.Kill("h1:s1:op2"); err != nil {
		t.Fatalf("kill: %v", err)
	}
	final, err := m.Wait(waitCtx, "h1:s1:op2", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if final.Background {
		t.Errorf("final should be completed after kill")
	}
	// 已终结：bg_list 为空
	if got := len(m.List()); got != 0 {
		t.Errorf("bg_list after kill = %d, want 0", got)
	}
	// bg_kill 已终结 → no such
	if err := m.Kill("h1:s1:op2"); err == nil || !strings.Contains(err.Error(), "no such") {
		t.Errorf("kill completed: %v", err)
	}
}

func TestStartUnknownProgram(t *testing.T) {
	m := NewManager(0)
	_, err := m.Start(context.Background(), StartOptions{
		ID: "x", LogPath: filepath.Join(t.TempDir(), "o.log"),
		Exec: []string{"definitely-not-a-program-xyz"},
	})
	if err == nil || !strings.Contains(err.Error(), "unknown action") {
		t.Errorf("err = %v", err)
	}
}

func TestReadHeadLinesTruncation(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "o.log")
	_ = os.WriteFile(logPath, []byte(strings.Repeat("x\n", MaxLines+10)), 0o644)
	m := NewManager(0)
	res := m.readResult(&Entry{ID: "t", LogPath: logPath}, false)
	if res.Lines != MaxLines || !res.Truncated {
		t.Errorf("lines=%d truncated=%v", res.Lines, res.Truncated)
	}
	if strings.Count(res.Content, "\n") != MaxLines {
		t.Errorf("content lines = %d", strings.Count(res.Content, "\n"))
	}
}
