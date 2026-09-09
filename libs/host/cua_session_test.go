package host

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCuaSessionEndedErr 校验"会话已结束"错误的判定（不漏报、不误报）。
func TestCuaSessionEndedErr(t *testing.T) {
	ended := errors.New("session 'mcp-1-2' has ended; tool call 'press_key' was rejected. " +
		"Call start_session with this id to revive it before issuing further actions, or use a new session id.")
	if !cuaSessionEndedErr(ended) {
		t.Fatal("ended-session error not detected")
	}
	if cuaSessionEndedErr(errors.New("tool press_key failed")) {
		t.Fatal("unrelated error misdetected")
	}
	if cuaSessionEndedErr(nil) {
		t.Fatal("nil error misdetected")
	}
}

// TestCuaFakeMcpHelper 是 helper 进程入口（充当假 cua-driver MCP server）。
func TestCuaFakeMcpHelper(t *testing.T) {
	if os.Getenv("AIC_FAKE_MCP") != "1" {
		t.Skip("helper process only")
	}
	fakeMcpMain()
	os.Exit(0) // 直接退出，避免测试框架向 MCP stdout 写额外内容
}

// TestCuaCallRevivesEndedSession 校验 call 在驱动会话结束后自动 start_session
// 重建（带显式 session label 时保留同名）并重试一次。
func TestCuaCallRevivesEndedSession(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "calls.log")
	cmd := exec.Command(os.Args[0], "-test.run=TestCuaFakeMcpHelper")
	cmd.Env = append(os.Environ(), "AIC_FAKE_MCP=1", "AIC_FAKE_MCP_LOG="+logPath)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
	}()

	m := newCuaMcp("fake", t.Logf)
	m.mu.Lock()
	m.cmd, m.stdin, m.alive = cmd, stdin, true
	m.mu.Unlock()
	go m.readLoop(stdout)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// 1) 带显式 session label：结束后 start_session 应带同名 label。
	res, err := m.call(ctx, "press_key", map[string]any{"key": "a", "session": "aic-s1"})
	if err != nil {
		t.Fatalf("call with session: %v", err)
	}
	if len(res.Content) == 0 || res.Content[0].Text != "done" {
		t.Fatalf("call with session: unexpected result %+v", res)
	}
	// 2) 无 session（隐式会话）：start_session 不应带 label。
	if _, err := m.call(ctx, "press_key", map[string]any{"key": "b"}); err != nil {
		t.Fatalf("call without session: %v", err)
	}

	got := strings.Join(readLines(t, logPath), "\n")
	want := strings.Join([]string{
		"press_key aic-s1",
		"start_session aic-s1",
		"press_key aic-s1",
		"press_key -",
		"start_session -",
		"press_key -",
	}, "\n")
	if got != want {
		t.Fatalf("driver call sequence mismatch\n got:\n%s\nwant:\n%s", got, want)
	}
}

// fakeMcpMain 极简 MCP server：每个会话名下的首个 press_key 报"会话已结束"，
// start_session 一律成功，其余调用成功；每次调用把 "name session" 追加到日志。
func fakeMcpMain() {
	logPath := os.Getenv("AIC_FAKE_MCP_LOG")
	pressCount := map[string]int{}
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req struct {
			ID     int             `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &req); err != nil || req.ID == 0 {
			continue
		}
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		json.Unmarshal(req.Params, &p)
		sess, _ := p.Arguments["session"].(string)
		if logPath != "" {
			if f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
				label := sess
				if label == "" {
					label = "-"
				}
				fmt.Fprintf(f, "%s %s\n", p.Name, label)
				f.Close()
			}
		}
		var result string
		switch {
		case p.Name == "press_key" && pressCount[sess] == 0:
			pressCount[sess]++
			result = `{"content":[{"type":"text","text":"session 'mcp-1-2' has ended; tool call 'press_key' was rejected. Call start_session with this id to revive it before issuing further actions, or use a new session id."}],"isError":true}`
		case p.Name == "start_session":
			result = `{"content":[{"type":"text","text":"session ready"}]}`
		default:
			result = `{"content":[{"type":"text","text":"done"}]}`
		}
		fmt.Fprintf(os.Stdout, "{\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":%s}\n", req.ID, result)
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read call log: %v", err)
	}
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// TestCuaLiveSessionRevive 真机集成：显式结束会话制造 "has ended"，验证
// call 自动 start_session 复活并重试（本机装了 cua-driver 才跑，只读动作）。
func TestCuaLiveSessionRevive(t *testing.T) {
	if findCuaDriver() == "" {
		t.Skip("cua-driver not installed")
	}
	initCuaRuntime(t.Logf)
	if cuaRt == nil {
		t.Skip("cua runtime not initialized")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, err := cuaRt.call(ctx, "start_session", map[string]any{}); err != nil {
		t.Fatalf("start_session: %v", err)
	}
	if _, err := cuaRt.call(ctx, "end_session", map[string]any{}); err != nil {
		t.Fatalf("end_session: %v", err)
	}
	// 会话已结束：普通动作会被驱动拒绝，call 应自动复活后重试成功。
	if _, err := cuaRt.call(ctx, "list_apps", map[string]any{}); err != nil {
		t.Fatalf("call after ended session (revive failed): %v", err)
	}
}
