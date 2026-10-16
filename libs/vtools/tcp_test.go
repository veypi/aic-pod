package vtools

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"
)

// tcpFixture 是一个行分隔 JSON 协议（vtools/tcp v1）的假服务：tools 回目录，
// call 回显 arguments；name=fail 回 isError；name=hang 不应答（测 ctx 取消）。
type tcpFixture struct {
	listener net.Listener
	tools    []Tool
}

func startTCPFixture(t *testing.T) *tcpFixture {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &tcpFixture{listener: listener, tools: []Tool{{Name: "echo", Description: "回显", InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`)}}}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go f.serve(conn)
		}
	}()
	t.Cleanup(func() { _ = listener.Close() })
	return f
}

func (f *tcpFixture) serve(conn net.Conn) {
	defer conn.Close()
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	for scanner.Scan() {
		var req struct {
			ID        int            `json:"id"`
			Method    string         `json:"method"`
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if json.Unmarshal(scanner.Bytes(), &req) != nil {
			continue
		}
		write := func(v map[string]any) {
			v["id"] = req.ID
			data, _ := json.Marshal(v)
			_, _ = conn.Write(append(data, '\n'))
		}
		switch req.Method {
		case "tools":
			write(map[string]any{"tools": f.tools})
		case "call":
			switch req.Name {
			case "fail":
				write(map[string]any{"result": map[string]any{"why": "boom"}, "isError": true})
			case "hang":
				// 丢弃不应答（模拟慢服务）；不阻塞本连接的后续请求
			default:
				write(map[string]any{"result": map[string]any{"echo": req.Arguments}})
			}
		}
	}
}

func (f *tcpFixture) addr() string { return f.listener.Addr().String() }

func TestTCPRoundtrip(t *testing.T) {
	f := startTCPFixture(t)
	p, err := NewTCP(f.addr())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tools, err := p.Tools(ctx)
	if err != nil || len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("tools: %+v %v", tools, err)
	}
	result, err := p.Call(ctx, "echo", map[string]any{"text": "hi"})
	if err != nil {
		t.Fatal(err)
	}
	echo, ok := result.(map[string]any)["echo"].(map[string]any)
	if !ok || echo["text"] != "hi" {
		t.Fatalf("call: %+v", result)
	}
	// isError → CallError（结果保留）
	result, err = p.Call(ctx, "fail", map[string]any{})
	var callErr *CallError
	if !errors.As(err, &callErr) {
		t.Fatalf("isError mapping: %v", err)
	}
	if why, _ := callErr.Result.(map[string]any)["why"].(string); why != "boom" {
		t.Fatalf("CallError result: %+v", callErr.Result)
	}
}

func TestTCPReconnectAfterClose(t *testing.T) {
	f := startTCPFixture(t)
	p, err := NewTCP(f.addr())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := p.Tools(ctx); err != nil {
		t.Fatal(err)
	}
	// 断开后下次调用自动重拨
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, err := p.Tools(ctx)
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("reconnect: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestTCPCallContextCancel(t *testing.T) {
	f := startTCPFixture(t)
	p, err := NewTCP(f.addr())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := p.Call(ctx, "hang", map[string]any{}); err == nil {
		t.Fatal("hang call must fail on ctx cancel")
	}
	// 取消不毁连接：后续调用照常
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	if _, err := p.Call(ctx2, "echo", map[string]any{}); err != nil {
		t.Fatalf("connection unusable after cancel: %v", err)
	}
}

func TestTCPInvalidAddress(t *testing.T) {
	for _, addr := range []string{"", "nohost", "127.0.0.1:", ":9999", "a:b:c"} {
		if _, err := NewTCP(addr); err == nil {
			t.Fatalf("accepted %q", addr)
		}
	}
}
