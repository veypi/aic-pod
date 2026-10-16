package vtools

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
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

// 旧代际 readLoop 的迟到收尾不能误杀 Close 后重拨的新连接（回归：fail 曾
// 无条件摘除当前连接——Close 置 nil → 重拨 → 旧 readLoop 退出时把新连接关掉）。
func TestTCPStaleFailDoesNotKillNewConn(t *testing.T) {
	f := startTCPFixture(t)
	p, err := NewTCP(f.addr())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := p.Tools(ctx); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	old := p.conn
	p.mu.Unlock()
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	// 重拨新一代并确认可用
	if _, err := p.Tools(ctx); err != nil {
		t.Fatalf("redial: %v", err)
	}
	// 旧连接的迟到 failConn 必须空转
	p.failConn(old, errors.New("stale readloop exit"))
	if _, err := p.Call(ctx, "echo", map[string]any{"text": "alive"}); err != nil {
		t.Fatalf("new connection killed by stale fail: %v", err)
	}
}

// Close 使在途请求立即失败（不再等调用方 ctx 超时）。
func TestTCPCloseFailsInflight(t *testing.T) {
	f := startTCPFixture(t)
	p, err := NewTCP(f.addr())
	if err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 1)
	go func() { // hang 永不应答；ctx 无超时
		_, err := p.Call(context.Background(), "hang", map[string]any{})
		errCh <- err
	}()
	// 等请求真正发出（进入 pending）
	deadline := time.Now().Add(2 * time.Second)
	for {
		p.mu.Lock()
		n := len(p.pending)
		p.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("request never went in-flight")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("in-flight call survived Close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight call hung after Close")
	}
}

// startTCPAuthFixture 是要求认证握手的服务端：首行必须是 {"auth":token}，
// 通过回 {"ok":true}；否则回 {"ok":false,...} 并断连（含「首行是 HTTP 报文」
// 的浏览器直连场景——无法驱动工具调用）。
func startTCPAuthFixture(t *testing.T, token string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				scanner := bufio.NewScanner(conn)
				if !scanner.Scan() {
					return
				}
				var hello struct {
					Auth string `json:"auth"`
				}
				if json.Unmarshal(scanner.Bytes(), &hello) != nil || hello.Auth != token {
					_, _ = conn.Write([]byte(`{"ok":false,"error":"auth failed"}` + "\n"))
					return
				}
				_, _ = conn.Write([]byte(`{"ok":true}` + "\n"))
				for scanner.Scan() {
					var req struct {
						ID     int    `json:"id"`
						Method string `json:"method"`
					}
					if json.Unmarshal(scanner.Bytes(), &req) != nil {
						continue
					}
					rep, _ := json.Marshal(map[string]any{"id": req.ID, "result": map[string]any{"ok": true}})
					_, _ = conn.Write(append(rep, '\n'))
				}
			}()
		}
	}()
	return listener.Addr().String()
}

func TestTCPAuthHandshake(t *testing.T) {
	addr := startTCPAuthFixture(t, "s3cret")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// 正确 token：握手后正常调用
	p, err := NewTCP(addr, WithToken("s3cret"))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err := p.Call(ctx, "ping", map[string]any{}); err != nil {
		t.Fatalf("auth + call: %v", err)
	}
	// 错误 token：被拒
	bad, err := NewTCP(addr, WithToken("wrong"))
	if err != nil {
		t.Fatal(err)
	}
	defer bad.Close()
	if _, err := bad.Call(ctx, "ping", map[string]any{}); err == nil || !strings.Contains(err.Error(), "auth rejected") {
		t.Fatalf("wrong token must be rejected: %v", err)
	}
	// 无 token：服务端要求认证，握手缺省 = 首行请求被拒
	plain, err := NewTCP(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	if _, err := plain.Call(ctx, "ping", map[string]any{}); err == nil {
		t.Fatal("token-less client must fail against auth-required server")
	}
	// 握手失败后能重拨重试（failConn 不钉死）
	if _, err := bad.Call(ctx, "ping", map[string]any{}); err == nil {
		t.Fatal("retry with wrong token must still fail")
	}
}

func TestTCPInvalidAddress(t *testing.T) {
	for _, addr := range []string{"", "nohost", "127.0.0.1:", ":9999", "a:b:c"} {
		if _, err := NewTCP(addr); err == nil {
			t.Fatalf("accepted %q", addr)
		}
	}
}
