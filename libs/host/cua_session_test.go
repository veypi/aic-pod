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

func fakeCuaMcp(t *testing.T, resetError bool) (*cuaMcp, string) {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "calls.log")
	cmd := exec.Command(os.Args[0], "-test.run=TestCuaFakeMcpHelper")
	cmd.Env = append(os.Environ(), "AIC_FAKE_MCP=1", "AIC_FAKE_MCP_LOG="+logPath)
	if resetError {
		cmd.Env = append(cmd.Env, "AIC_FAKE_MCP_RESET_ERROR=1")
	}
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
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})

	m := newCuaMcp("fake", t.Logf)
	m.mu.Lock()
	m.cmd, m.stdin, m.alive = cmd, stdin, true
	m.mu.Unlock()
	go m.readLoop(stdout)
	return m, logPath
}

// A failed action is never replayed; only the next command restores the implicit session.
func TestCuaCallDoesNotReplayEndedSession(t *testing.T) {
	m, logPath := fakeCuaMcp(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	initialEpoch := m.generation
	if _, err := m.call(ctx, "press_key", map[string]any{"key": "a", "session": "aic-s1"}); err == nil {
		t.Fatal("ended session must fail without replay")
	}
	got := strings.Join(readLines(t, logPath), "\n")
	if got != "press_key aic-s1" {
		t.Fatalf("unexpected replay: %s", got)
	}

	previousEpoch := m.generation
	if previousEpoch == initialEpoch {
		t.Fatal("session expiration did not invalidate UI bindings")
	}
	if _, err := m.callEpoch(ctx, initialEpoch, "press_key", map[string]any{"key": "a"}); err == nil {
		t.Fatal("expired epoch accepted input")
	}
	if _, err := m.call(ctx, "list_apps", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(readLines(t, logPath), "\n"); got != "press_key aic-s1\nstart_session -\nlist_apps -" {
		t.Fatalf("wrong recovery sequence: %s", got)
	}
	if m.generation != previousEpoch {
		t.Fatal("reset must not rebind old handles")
	}
	if m.needsSessionReset {
		t.Fatal("connection remains expired")
	}
}

func TestCuaRecoveryFailureDoesNotDispatch(t *testing.T) {
	m, logPath := fakeCuaMcp(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := m.call(ctx, "press_key", map[string]any{"key": "a"}); err == nil {
		t.Fatal("expected session expiration")
	}
	for range 2 {
		if _, err := m.call(ctx, "list_apps", map[string]any{}); err == nil || !strings.Contains(err.Error(), "session recovery") {
			t.Fatalf("expected failed recovery: %v", err)
		}
	}
	if got := strings.Join(readLines(t, logPath), "\n"); got != "press_key -\nstart_session -\nstart_session -" {
		t.Fatalf("dispatched through failed recovery: %s", got)
	}
	if !m.needsSessionReset {
		t.Fatal("forgot unsuccessful recovery")
	}
}

// The first press poisons the implicit transport session; all tools then fail
// until start_session revives it. Each call is recorded to detect replay.
func fakeMcpMain() {
	logPath := os.Getenv("AIC_FAKE_MCP_LOG")
	pressCount := map[string]int{}
	expired := false
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
		case p.Name == "start_session" && os.Getenv("AIC_FAKE_MCP_RESET_ERROR") == "1":
			result = `{"content":[{"type":"text","text":"driver is unavailable"}],"isError":true}`
		case p.Name == "start_session":
			expired = false
			result = `{"content":[{"type":"text","text":"session ready"}]}`
		case expired || (p.Name == "press_key" && pressCount[sess] == 0):
			pressCount[sess]++
			expired = true
			result = `{"content":[{"type":"text","text":"session 'mcp-1-2' has ended; tool call 'press_key' was rejected. Call start_session with this id to revive it before issuing further actions, or use a new session id."}],"isError":true}`
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
