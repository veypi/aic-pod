// Copyright (C) 2025 veypi <i@veypi.com>
// Distributed under terms of the MIT license.

// Package skillproc 是 skill service provider 与 pod 之间的最小有线协议
// （aic/docs/skill.md §9.2，2026-10-01 定）：
//
//	帧 = [4B 大端头长][JSON 头][负载字节]（头 len 字段 = 负载字节数）
//
// 单连接多路复用（id 关联请求/输出/取消/stream 通道）。只提供
// invoke/frame/exit/cancel 与 stream.open/stream.frame/stream.close/error；
// 无 handshake、health、版本协商、幂等缓存或自动恢复——启动/停止由进程
// 管理（bg 任务表）完成，取消不承诺撤销已发生的副作用。
//
// 本模块是纯编解码 + 连接读写，零业务知识：pod 核心（libs/skillrun）与
// provider 双向 import 它，provider 不 import pod 的 cfg/host/runtime。
package skillproc

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync"
)

// 帧类型（Header.Type）。
const (
	// pod → provider：一次调用。Argv/Cwd/Env 描述执行请求；负载 = stdin
	// 字节（无 stdin 时 len=0）。响应 = 若干 Frame + 一个 Exit（或 Error）。
	TypeInvoke = "invoke"
	// provider → pod：调用的 stdout/stderr 数据帧（Stream 区分），可多次。
	TypeFrame = "frame"
	// provider → pod：调用终态。Code = exit_code；Error 非空 = 执行失败。
	TypeExit = "exit"
	// pod → provider：取消当前调用（关联 ID；不承诺撤销已发生副作用）。
	TypeCancel = "cancel"
	// 双向：打开一条 stream 二进制通道（Name = manifest 声明的 stream 名；
	// ID 自此标识该通道）。对端以 StreamFrame/StreamClose/Error 应答。
	TypeStreamOpen = "stream.open"
	// 双向：stream 通道数据帧（负载 = 不透明字节，不逐帧 JSON/base64）。
	TypeStreamFrame = "stream.frame"
	// 双向：关闭 stream 通道（关闭流就是关闭通道；Error 携带终止原因）。
	TypeStreamClose = "stream.close"
	// 双向：请求级错误（方法不存在、参数不支持等实际错误，如实返回）。
	TypeError = "error"
)

// Frame 的 Stream 取值（TypeFrame）。
const (
	StreamStdout = "stdout"
	StreamStderr = "stderr"
)

// 防御上限（协议不关心业务大小，只防对端垃圾/内存炸弹）。
const (
	MaxHeader  = 64 << 10  // JSON 头 64KB
	MaxPayload = 256 << 20 // 单帧负载 256MB
)

// Header 是帧的 JSON 头。字段按帧类型取用（见类型常量注释）。
type Header struct {
	V      int               `json:"v"`                // 协议结构版本（恒 1；非协商，仅供未来解析分流）
	ID     string            `json:"id"`               // 请求/通道关联键
	Type   string            `json:"type"`             // 帧类型（见常量）
	Len    int               `json:"len,omitempty"`    // 负载字节数（0 = 无负载）
	Stream string            `json:"stream,omitempty"` // frame：stdout|stderr
	Argv   []string          `json:"argv,omitempty"`   // invoke
	Cwd    string            `json:"cwd,omitempty"`    // invoke
	Env    map[string]string `json:"env,omitempty"`    // invoke
	Name   string            `json:"name,omitempty"`   // stream.open：manifest stream 名
	Code   int               `json:"code,omitempty"`   // exit：exit_code
	Error  string            `json:"error,omitempty"`  // exit/stream.close/error
}

// WriteFrame 写一帧（头 + 负载，负载可为 nil）。写方并发安全由 Conn 保证；
// 裸用本函数的调用方自行串行化写端。
func WriteFrame(w io.Writer, h Header, payload []byte) error {
	if h.V == 0 {
		h.V = 1
	}
	h.Len = len(payload)
	hb, err := json.Marshal(h)
	if err != nil {
		return fmt.Errorf("skillproc: marshal header: %w", err)
	}
	if len(hb) > MaxHeader {
		return fmt.Errorf("skillproc: header too large: %d > %d", len(hb), MaxHeader)
	}
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(len(hb)))
	if _, err := w.Write(prefix[:]); err != nil {
		return err
	}
	if _, err := w.Write(hb); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

// ReadFrame 读一帧。返回的 payload 为 nil 表示无负载；io.EOF 表示对端
// 在帧边界干净关闭，io.ErrUnexpectedEOF 表示半帧断开。
func ReadFrame(r io.Reader) (Header, []byte, error) {
	var prefix [4]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		return Header{}, nil, err
	}
	hl := binary.BigEndian.Uint32(prefix[:])
	if hl == 0 || hl > MaxHeader {
		return Header{}, nil, fmt.Errorf("skillproc: bad header length %d", hl)
	}
	hb := make([]byte, hl)
	if _, err := io.ReadFull(r, hb); err != nil {
		return Header{}, nil, err
	}
	var h Header
	if err := json.Unmarshal(hb, &h); err != nil {
		return Header{}, nil, fmt.Errorf("skillproc: bad header: %w", err)
	}
	if h.Len < 0 || h.Len > MaxPayload {
		return Header{}, nil, fmt.Errorf("skillproc: bad payload length %d", h.Len)
	}
	if h.Len == 0 {
		return h, nil, nil
	}
	payload := make([]byte, h.Len)
	if _, err := io.ReadFull(r, payload); err != nil {
		return Header{}, nil, err
	}
	return h, payload, nil
}

// Conn 是一条 skillproc 连接（unix socket / named pipe）：写端串行化
// （多 goroutine 可并发 Send——stdout/stderr/stream 各写者不交叠帧）；
// 读端单读者（Recv 由接收循环独占，demux 是调用方职责）。
type Conn struct {
	conn net.Conn
	wmu  sync.Mutex
}

// NewConn 包装已建立的连接。
func NewConn(c net.Conn) *Conn { return &Conn{conn: c} }

// Dial 连接 unix socket 对端。
func Dial(network, addr string) (*Conn, error) {
	c, err := net.Dial(network, addr)
	if err != nil {
		return nil, err
	}
	return NewConn(c), nil
}

// Send 写一帧（并发安全）。
func (c *Conn) Send(h Header, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return WriteFrame(c.conn, h, payload)
}

// Recv 读一帧（单读者）。
func (c *Conn) Recv() (Header, []byte, error) {
	return ReadFrame(c.conn)
}

// Close 关闭底层连接。
func (c *Conn) Close() error { return c.conn.Close() }
