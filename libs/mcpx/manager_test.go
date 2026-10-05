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

func TestManagerRejectsInvalidConfiguration(t *testing.T) {
	for _, cfg := range []Config{{}, {Command: "tool", URL: "https://example.test/mcp"}, {Command: "tool", Cwd: "relative"}, {URL: "file:///tmp/mcp"}, {URL: "https://example.test/mcp", Env: map[string]string{"TOKEN": "secret"}}} {
		if _, err := NewManager(map[string]Config{"fixture": cfg}, Options{Processes: vbox.NewManager()}); err == nil {
			t.Fatalf("accepted %+v", cfg)
		}
	}
}
