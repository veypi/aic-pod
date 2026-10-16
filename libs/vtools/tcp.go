package vtools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"
)

// TCP 行分隔 JSON 协议（vtools/tcp v1）——非 MCP 协议的样例适配器：
//
//	→ {"id":1,"method":"tools"}
//	← {"id":1,"tools":[{"name":"echo","description":"…","inputSchema":{…}}]}
//	→ {"id":2,"method":"call","name":"echo","arguments":{"text":"hi"}}
//	← {"id":2,"result":…,"isError":false}   或   {"id":2,"error":"…"}
//
// 一条连接懒建立并复用；读循环按 id 分发；连接断开使全部在途请求失败，
// 下次调用自动重拨。调用方 ctx 只控制等待，不持有连接。
type TCPProvider struct {
	addr string

	mu      sync.Mutex
	conn    net.Conn
	pending map[int]chan tcpOutcome
	nextID  int
}

type tcpReply struct {
	Result  json.RawMessage `json:"result"`
	Tools   []Tool          `json:"tools"`
	IsError bool            `json:"isError"`
	Err     string          `json:"error"`
}

type tcpOutcome struct {
	rep tcpReply
	err error
}

// NewTCP 构造 TCP 适配器（addr = host:port）。
func NewTCP(addr string) (*TCPProvider, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host == "" || port == "" {
		return nil, fmt.Errorf("vtools tcp: invalid address %q (want host:port)", addr)
	}
	return &TCPProvider{addr: addr, pending: map[int]chan tcpOutcome{}}, nil
}

func (p *TCPProvider) Tools(ctx context.Context) ([]Tool, error) {
	rep, err := p.roundtrip(ctx, map[string]any{"method": "tools"})
	if err != nil {
		return nil, err
	}
	return rep.Tools, nil
}

func (p *TCPProvider) Call(ctx context.Context, tool string, args map[string]any) (any, error) {
	rep, err := p.roundtrip(ctx, map[string]any{"method": "call", "name": tool, "arguments": args})
	if err != nil {
		return nil, err
	}
	var result any
	if len(rep.Result) > 0 {
		if err := json.Unmarshal(rep.Result, &result); err != nil {
			return nil, fmt.Errorf("vtools tcp %s: result is not valid JSON: %w", p.addr, err)
		}
	}
	if rep.IsError {
		return result, &CallError{Result: result}
	}
	return result, nil
}

// Addr 返回配置地址（登记条目与配置比对用）。
func (p *TCPProvider) Addr() string { return p.addr }

// Close 断开连接（在途请求由 readLoop 的 fail 收尾）；下次调用自动重拨。幂等。
func (p *TCPProvider) Close() error {
	p.mu.Lock()
	conn := p.conn
	p.conn = nil // 立即摘除：窗口期内的下一次调用直接重拨，而不是写到已关闭连接
	p.mu.Unlock()
	if conn != nil {
		return conn.Close()
	}
	return nil
}

// roundtrip 发一条请求并等待同 id 应答。写串行（mu），读按 id 分发。
func (p *TCPProvider) roundtrip(ctx context.Context, req map[string]any) (*tcpReply, error) {
	p.mu.Lock()
	if p.conn == nil {
		conn, err := net.DialTimeout("tcp", p.addr, 10*time.Second)
		if err != nil {
			p.mu.Unlock()
			return nil, fmt.Errorf("vtools tcp %s: dial: %w", p.addr, err)
		}
		p.conn = conn
		go p.readLoop(conn)
	}
	p.nextID++
	id := p.nextID
	req["id"] = id
	data, err := json.Marshal(req)
	if err != nil {
		p.mu.Unlock()
		return nil, err
	}
	data = append(data, '\n')
	ch := make(chan tcpOutcome, 1)
	p.pending[id] = ch
	conn := p.conn
	_, werr := conn.Write(data)
	p.mu.Unlock()
	if werr != nil {
		p.fail(fmt.Errorf("vtools tcp %s: write: %w", p.addr, werr))
		return nil, werr
	}
	select {
	case <-ctx.Done():
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
		return nil, ctx.Err()
	case outcome := <-ch:
		if outcome.err != nil {
			return nil, outcome.err
		}
		if outcome.rep.Err != "" {
			return nil, fmt.Errorf("vtools tcp %s: %s", p.addr, outcome.rep.Err)
		}
		return &outcome.rep, nil
	}
}

func (p *TCPProvider) readLoop(conn net.Conn) {
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	for scanner.Scan() {
		var rep struct {
			ID int `json:"id"`
			tcpReply
		}
		if err := json.Unmarshal(scanner.Bytes(), &rep); err != nil {
			continue // 坏行不致命：跳过（无 id 可路由）
		}
		p.mu.Lock()
		ch := p.pending[rep.ID]
		delete(p.pending, rep.ID)
		p.mu.Unlock()
		if ch != nil {
			ch <- tcpOutcome{rep: rep.tcpReply}
		}
	}
	err := scanner.Err()
	if err == nil {
		err = fmt.Errorf("vtools tcp %s: connection closed", p.addr)
	}
	p.fail(err)
}

// fail 断开连接并使全部在途请求失败；之后的调用重拨。
func (p *TCPProvider) fail(err error) {
	p.mu.Lock()
	conn := p.conn
	p.conn = nil
	pending := p.pending
	p.pending = map[int]chan tcpOutcome{}
	p.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
	for _, ch := range pending {
		ch <- tcpOutcome{err: err}
	}
}
