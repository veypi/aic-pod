package mcpx

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/veypi/vbox"
)

func TestMCPProcessFixture(t *testing.T) {
	if os.Getenv("AIC_MCP_TEST_PROCESS") != "1" {
		return
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	var mu sync.Mutex
	count := 0
	mcp.AddTool(s, &mcp.Tool{Name: "next", Description: "Persistent state"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, map[string]any, error) {
		mu.Lock()
		defer mu.Unlock()
		count++
		cwd, _ := os.Getwd()
		return nil, map[string]any{"count": count, "pid": os.Getpid(), "cwd": cwd, "env": os.Getenv("AIC_MCP_FIXED")}, nil
	})
	mcp.AddTool(s, &mcp.Tool{Name: "wait", Description: "Wait until cancelled"}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, map[string]any, error) {
		<-ctx.Done()
		return nil, nil, ctx.Err()
	})
	mcp.AddTool(s, &mcp.Tool{Name: "slow", Description: "Return after two seconds"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, map[string]any, error) {
		time.Sleep(2 * time.Second)
		return nil, map[string]any{"pid": os.Getpid()}, nil
	})
	if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}
func TestManagerOwnsProcessAcrossCallerCancellation(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	processes := vbox.NewManager()
	manager, err := NewManager(map[string]Config{"fixture": {Command: binary, Args: []string{"-test.run=^TestMCPProcessFixture$"}, Cwd: cwd, Env: map[string]string{"AIC_MCP_TEST_PROCESS": "1", "AIC_MCP_FIXED": "configured"}, NoSandbox: true}}, Options{Processes: processes, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	caller, stop := context.WithCancel(ctx)
	cs, err := manager.Session(caller, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	stop()
	read := func(client *mcp.ClientSession) map[string]any {
		t.Helper()
		r, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "next", Arguments: map[string]any{}})
		if err != nil {
			t.Fatal(err)
		}
		var v map[string]any
		data, err := json.Marshal(r.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(data, &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	first := read(cs)
	if first["count"] != float64(1) || first["env"] != "configured" || first["cwd"] != cwd {
		t.Fatal(first)
	}
	deadline, end := context.WithTimeout(ctx, 50*time.Millisecond)
	defer end()
	if _, err = cs.CallTool(deadline, &mcp.CallToolParams{Name: "wait", Arguments: map[string]any{}}); err == nil {
		t.Fatal("wait was not cancelled")
	}
	again, err := manager.Session(ctx, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if again != cs {
		t.Fatal("caller cancellation replaced shared connection")
	}
	second := read(again)
	if second["count"] != float64(2) || second["pid"] != first["pid"] {
		t.Fatal(second)
	}
	if err = manager.Restart(ctx, "fixture"); err != nil {
		t.Fatal(err)
	}
	replacement, err := manager.Session(ctx, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if value := read(replacement); value["count"] != float64(1) || value["pid"] == first["pid"] {
		t.Fatal(value)
	}
}

// 主动收尾（Close/Restart）是控制行为，不是故障：连接结束与退出码均不进日志；
// 只有服务自行退出/崩溃才记录。
func TestManagerShutdownIsQuiet(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var logs []string
	manager, err := NewManager(map[string]Config{"fixture": {Command: binary, Args: []string{"-test.run=^TestMCPProcessFixture$"}, Cwd: t.TempDir(), Env: map[string]string{"AIC_MCP_TEST_PROCESS": "1"}, NoSandbox: true}}, Options{Processes: vbox.NewManager(), Logf: func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, fmt.Sprintf(format, args...))
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := manager.Session(ctx, "fixture"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Restart(ctx, "fixture"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, line := range logs {
		if strings.Contains(line, "connection ended") || strings.Contains(line, "process exited") || strings.Contains(line, "process failed") {
			t.Fatalf("shutdown noise: %s", line)
		}
	}
}

// 空闲有效期：无调用到期回收（下一次调用建新进程）；在途请求算活跃，
// 不因超时把长调用从中间砍断。
func TestManagerIdleTimeoutReleasesService(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var expired []string
	manager, err := NewManager(
		map[string]Config{"fixture": {Command: binary, Args: []string{"-test.run=^TestMCPProcessFixture$"}, Cwd: t.TempDir(), Env: map[string]string{"AIC_MCP_TEST_PROCESS": "1"}, IdleTimeout: "1s", NoSandbox: true}},
		Options{Processes: vbox.NewManager(), Logf: t.Logf, Expired: func(name string) {
			mu.Lock()
			defer mu.Unlock()
			expired = append(expired, name)
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := func() float64 {
		t.Helper()
		session, err := manager.Session(ctx, "fixture")
		if err != nil {
			t.Fatal(err)
		}
		r, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "next", Arguments: map[string]any{}})
		if err != nil {
			t.Fatal(err)
		}
		var v map[string]any
		data, err := json.Marshal(r.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(data, &v); err != nil {
			t.Fatal(err)
		}
		return v["pid"].(float64)
	}
	if first := pid(); first == 0 {
		t.Fatal("no pid")
	} else {
		// 在途的长调用（2s > 1s 有效期）必须存活到返回。
		session, err := manager.Session(ctx, "fixture")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "slow", Arguments: map[string]any{}}); err != nil {
			t.Fatalf("in-flight call was released: %v", err)
		}
		waitIdleRelease(t, manager, "fixture")
		if second := pid(); second == first {
			t.Fatalf("idle timeout did not release the process (pid %v)", second)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(expired) != 1 || expired[0] != "fixture" {
		t.Fatalf("expired callbacks: %v", expired)
	}
}

// Touch 续期：UI 实时画面等不经过 MCP session 的持用同样算活跃。
func TestManagerTouchKeepsServiceAlive(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(map[string]Config{"fixture": {Command: binary, Args: []string{"-test.run=^TestMCPProcessFixture$"}, Cwd: t.TempDir(), Env: map[string]string{"AIC_MCP_TEST_PROCESS": "1"}, IdleTimeout: "1s", NoSandbox: true}}, Options{Processes: vbox.NewManager(), Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	session, err := manager.Session(ctx, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(1500 * time.Millisecond); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
		manager.Touch("fixture")
	}
	manager.mu.Lock()
	current := manager.instances["fixture"]
	manager.mu.Unlock()
	if current == nil {
		t.Fatal("touched service was released")
	}
	waitIdleRelease(t, manager, "fixture")
	if again, err := manager.Session(ctx, "fixture"); err != nil {
		t.Fatal(err)
	} else if again == session {
		t.Fatal("released service was reused")
	}
}

// waitIdleRelease 直接看内部表，不用 Session 探测（探测本身算使用、会续期）。
func waitIdleRelease(t *testing.T, manager *Manager, name string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		manager.mu.Lock()
		_, present := manager.instances[name]
		manager.mu.Unlock()
		if !present {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("mcp %s: service still running past its idle timeout", name)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestManagerRejectsInvalidConfiguration(t *testing.T) {
	for _, cfg := range []Config{{}, {Command: "tool", URL: "https://example.test/mcp"}, {Command: "tool", Cwd: "relative"}, {URL: "file:///tmp/mcp"}, {URL: "https://example.test/mcp", Env: map[string]string{"TOKEN": "secret"}}, {Command: "tool", IdleTimeout: "soon"}, {Command: "tool", IdleTimeout: "-1m"}} {
		if _, err := NewManager(map[string]Config{"fixture": cfg}, Options{Processes: vbox.NewManager()}); err == nil {
			t.Fatalf("accepted %+v", cfg)
		}
	}
}
