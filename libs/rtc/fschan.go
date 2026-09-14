package rtc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/veypi/aic-pod/libs/proto"
	"github.com/veypi/aic-pod/libs/vcore"
)

// fsChannelLabel 是 fs 数据通道的协商标签（页面 createDataChannel("fs")）。
const fsChannelLabel = "fs"

// 帧协议 v1（全 JSON 文本帧；并发口径：响应/chunk 帧带请求 id 按 id 分流，
// 设备端每请求独立 goroutine 执行、发送由 sendMu 串行——readbin 可并发，
// fs 调用客户端保持串行）：
//
//	鉴权：{id, op:"auth", code} → {op:"auth_ok", host_id,...} / {op:"auth_err", error}
//	请求：{id, op:"fs", args{action,...}}——args 即 vcore.RunFS 原生输入；
//	      载荷超 inlineLimit（典型 = write 的 content）时 args 剥离 content、
//	      stream:true，随后 {op:"chunk", seq, text}×N + {op:"end"}；
//	      设备端重组后把 content 注回 args 再执行。
//	      {id, op:"readbin", args{path, off?, len?}}——readbin 是直连控制台私有
//	      op（2026-09-10，不属于 fs 指令集）：原始字节的唯一出口，供预览/下载
//	      大二进制（视频等）；执行体 = vcore.ReadBin（fsauth 同一判定实例）。
//	      {id, op:"writebin", args{path, size?}}——writebin 是写方向私有 op
//	      （2026-09-12，与 readbin 对称）：text/chunk 载荷 = 文件字节的 base64
//	      （≤inlineLimit 随 head 内联 text；超限 stream:true + chunk×N + end），
//	      设备端重组解码后经 vcore.WriteBin 整文件落盘（fsauth write 级判定）。
//	响应：{id, op:"result", ok:true, data:{content,attrs}}（≤inlineLimit 内联）；
//	      超限则 stream:true + chunk×N + end（data 为重组后的 JSON 文本）；
//	      错误：{op:"result", ok:false, error, state}（state = proto.StateOf）。
//	      readbin 响应带 bin:true + attrs{mime,size,total}：base64 ≤ inlineLimit
//	      走 text 内联，超限走 stream（chunk.text = base64 段，重组后整体解码）。
//
// 消息尺寸纪律：chunk 文本载荷 ≤48KB（远低于 Chrome DataChannel 单消息上限）；
// 发送侧 BufferedAmount 超 8MB 高水位时等待（backpressure）。
const (
	frameInlineLimit = 32 << 10
	frameChunkSize   = 48 << 10
	bufHighWater     = 8 << 20
	authWindow       = 5 * time.Second
)

// fsFrame 是通道上的统一帧结构（op 区分语义，字段按 op 取用）。
// fs 请求的 Args 即 vcore.RunFS 原生输入（含 action 字段），不拆包。
type fsFrame struct {
	ID       string            `json:"id"`
	Op       string            `json:"op"`
	Code     string            `json:"code,omitempty"`
	Args     json.RawMessage   `json:"args,omitempty"`
	Stream   bool              `json:"stream,omitempty"`
	Bin      bool              `json:"bin,omitempty"` // readbin 响应标记（text/chunk 载荷 = base64）
	Size     int               `json:"size,omitempty"`
	Seq      int               `json:"seq,omitempty"`
	Text     string            `json:"text,omitempty"`
	OK       bool              `json:"ok,omitempty"`
	Error    string            `json:"error,omitempty"`
	State    string            `json:"state,omitempty"`
	Attrs    map[string]string `json:"attrs,omitempty"` // readbin：mime/size/total
	Data     json.RawMessage   `json:"data,omitempty"`
	HostID   string            `json:"host_id,omitempty"`
	Hostname string            `json:"hostname,omitempty"`
	Version  string            `json:"version,omitempty"`
}

// streamBuf 是一个请求/响应的 chunk 重组缓冲。
type streamBuf struct {
	args json.RawMessage // 请求头携带的参数（fs 不含 content；writebin 为 args{path}）
	buf  []byte
	bin  bool // writebin 流：end 时走 executeWriteBin（base64 解码）而非 fs 注回
}

// fsChannel 是一个已协商 DataChannel 的服务状态机。
type fsChannel struct {
	svc *Service
	dc  *webrtc.DataChannel

	sendMu sync.Mutex // 发送串行化（执行 goroutine 并发完成，发送不乱序）

	mu      sync.Mutex
	authed  bool
	streams map[string]*streamBuf
}

// serveFSChannel 接管一个协商成功的 fs 通道（auth 窗口内未完成鉴权即断开）。
func (s *Service) serveFSChannel(pcID string, dc *webrtc.DataChannel) {
	ch := &fsChannel{svc: s, dc: dc, streams: map[string]*streamBuf{}}
	dc.OnOpen(func() {
		s.logf("rtc: pc=%s fs channel open (await auth)", pcID)
		time.AfterFunc(authWindow, func() {
			ch.mu.Lock()
			authed := ch.authed
			ch.mu.Unlock()
			if !authed {
				s.logf("rtc: pc=%s auth window expired, closing", pcID)
				_ = dc.Close()
			}
		})
	})
	dc.OnClose(func() {
		// fs 通道是 PC 的唯一用途：通道一关即回收整个连接（2026-09-10 评审）。
		// 否则未 authed 的连接（auth 窗口只关 dc）会一直滞留在 pcs 里。
		s.logf("rtc: pc=%s fs channel closed, dropping pc", pcID)
		s.dropPC(pcID)
	})
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		if !msg.IsString {
			return // v1 协议只定义文本帧
		}
		var f fsFrame
		if err := json.Unmarshal(msg.Data, &f); err != nil || f.ID == "" {
			return
		}
		ch.onFrame(&f)
	})
}

func (ch *fsChannel) onFrame(f *fsFrame) {
	ch.mu.Lock()
	authed := ch.authed
	ch.mu.Unlock()

	if !authed {
		if f.Op != "auth" {
			return
		}
		ch.handleAuth(f)
		return
	}

	switch f.Op {
	case "fs":
		if f.Stream {
			ch.mu.Lock()
			ch.streams[f.ID] = &streamBuf{args: f.Args}
			ch.mu.Unlock()
			return
		}
		go ch.execute(f.ID, f.Args)
	case "readbin":
		// 直连控制台私有 op（非 fs 指令集）：参数恒小载荷，无流式请求形态
		go ch.executeBin(f.ID, f.Args)
	case "writebin":
		// 写方向私有 op（2026-09-12）：text 内联载荷，或先收 head（stream:true）
		// 再 chunk×N + end（载荷 = 文件字节 base64）
		if f.Stream {
			ch.mu.Lock()
			ch.streams[f.ID] = &streamBuf{args: f.Args, bin: true}
			ch.mu.Unlock()
			return
		}
		go ch.executeWriteBin(f.ID, f.Args, f.Text)
	case "chunk":
		ch.mu.Lock()
		if sb, ok := ch.streams[f.ID]; ok {
			sb.buf = append(sb.buf, f.Text...)
		}
		ch.mu.Unlock()
	case "end":
		ch.mu.Lock()
		sb, ok := ch.streams[f.ID]
		delete(ch.streams, f.ID)
		ch.mu.Unlock()
		if !ok {
			return
		}
		if sb.bin {
			go ch.executeWriteBin(f.ID, sb.args, string(sb.buf))
			return
		}
		go ch.execute(f.ID, injectContent(sb.args, string(sb.buf)))
	default:
		// auth 重发/未知 op：忽略
	}
}

// handleAuth 校验鉴权帧：code 比对（与本地管理 API x-aic-code 同源）+
// 5 次失败锁 1 分钟（与 api 包 security 同语义，独立计数）。
func (ch *fsChannel) handleAuth(f *fsFrame) {
	if !ch.svc.checkCode(f.Code) {
		_ = ch.sendFrame(&fsFrame{ID: f.ID, Op: "auth_err", Error: "invalid or locked code"})
		_ = ch.dc.Close()
		return
	}
	ch.mu.Lock()
	ch.authed = true
	ch.mu.Unlock()
	_ = ch.sendFrame(&fsFrame{
		ID: f.ID, Op: "auth_ok", OK: true,
		HostID: ch.svc.cfg.HostID, Hostname: ch.svc.cfg.Hostname, Version: ch.svc.cfg.Version,
	})
}

// checkCode 校验 code（未锁定且相等）；连续 5 次失败锁 1 分钟。
func (s *Service) checkCode(code string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if now.Before(s.lockEnd) {
		return false
	}
	if code != s.cfg.Code {
		s.failCnt++
		if s.failCnt >= 5 {
			s.lockEnd = now.Add(time.Minute)
			s.failCnt = 0
		}
		return false
	}
	s.failCnt = 0
	return true
}

// ---- readbin（2026-09-10，直连控制台私有 op，不属于 fs 指令集） ----

// readbinArgs 是 readbin op 的参数：path 必填；off/len 为字节区间，len<=0 = 到文件尾。
type readbinArgs struct {
	Path string `json:"path"`
	Off  int64  `json:"off,omitempty"`
	Len  int64  `json:"len,omitempty"`
}

// executeBin 执行 readbin 并回送原始字节（base64 文本帧，v1；二进制帧留 v2）。
// 内联阈值与 fs 响应同值；超限走 stream:true + chunk×N + end（重组后整体解码）。
func (ch *fsChannel) executeBin(id string, raw json.RawMessage) {
	var a readbinArgs
	if err := json.Unmarshal(raw, &a); err != nil || a.Path == "" {
		_ = ch.sendFrame(&fsFrame{ID: id, Op: "result", OK: false, Error: "readbin: invalid args (path required)"})
		return
	}
	data, mime, total, err := ch.svc.cfg.ReadBin(a.Path, a.Off, a.Len)
	if err != nil {
		_ = ch.sendFrame(&fsFrame{ID: id, Op: "result", OK: false,
			Error: err.Error(), State: string(proto.StateOf(err))})
		return
	}
	attrs := map[string]string{
		"mime":  mime,
		"size":  fmt.Sprint(len(data)),
		"total": fmt.Sprint(total),
	}
	b64 := base64.StdEncoding.EncodeToString(data)
	if len(b64) <= frameInlineLimit {
		_ = ch.sendFrame(&fsFrame{ID: id, Op: "result", OK: true, Bin: true, Text: b64, Attrs: attrs})
		return
	}
	if err := ch.sendFrame(&fsFrame{ID: id, Op: "result", OK: true, Stream: true, Bin: true, Size: len(b64), Attrs: attrs}); err != nil {
		return
	}
	for seq, off := 0, 0; off < len(b64); seq, off = seq+1, off+frameChunkSize {
		end := off + frameChunkSize
		if end > len(b64) {
			end = len(b64)
		}
		if err := ch.sendFrame(&fsFrame{ID: id, Op: "chunk", Seq: seq, Text: b64[off:end]}); err != nil {
			return
		}
	}
	_ = ch.sendFrame(&fsFrame{ID: id, Op: "end"})
}

// ---- writebin（2026-09-12，直连控制台私有 op，写方向，与 readbin 对称） ----

// writebinArgs 是 writebin op 的参数：path 必填；size = 文件字节数（信息位，
// 供设备端预检；最终以解码后字节为准）。
type writebinArgs struct {
	Path string `json:"path"`
	Size int64  `json:"size,omitempty"`
}

// executeWriteBin 把 base64 文本载荷解码为原始字节整写入文件（vcore.WriteBin，
// fsauth write 级判定照常；deny 恒拒）。载荷内联（head.text ≤inlineLimit）或
// 流式（stream:true 先到 + chunk×N + end，重组后整体解码）两种形态。
func (ch *fsChannel) executeWriteBin(id string, raw json.RawMessage, b64 string) {
	var a writebinArgs
	if err := json.Unmarshal(raw, &a); err != nil || a.Path == "" {
		_ = ch.sendFrame(&fsFrame{ID: id, Op: "result", OK: false, Error: "writebin: invalid args (path required)"})
		return
	}
	if a.Size > vcore.MaxWriteBinBytes {
		_ = ch.sendFrame(&fsFrame{ID: id, Op: "result", OK: false,
			Error: fmt.Sprintf("writebin: size %d exceeds max %d", a.Size, vcore.MaxWriteBinBytes)})
		return
	}
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		_ = ch.sendFrame(&fsFrame{ID: id, Op: "result", OK: false, Error: "writebin: bad base64 payload"})
		return
	}
	n, err := ch.svc.cfg.WriteBin(a.Path, data)
	if err != nil {
		_ = ch.sendFrame(&fsFrame{ID: id, Op: "result", OK: false,
			Error: err.Error(), State: string(proto.StateOf(err))})
		return
	}
	payload, err := json.Marshal(&vcore.Result{
		Content: fmt.Sprintf("wrote file: %s (%d bytes)", a.Path, n),
		Attrs: map[string]string{
			"bytes": fmt.Sprint(n),
			"size":  fmt.Sprint(n),
			"path":  a.Path,
		},
	})
	if err != nil {
		_ = ch.sendFrame(&fsFrame{ID: id, Op: "result", OK: false, Error: fmt.Sprintf("marshal result: %v", err)})
		return
	}
	_ = ch.sendFrame(&fsFrame{ID: id, Op: "result", OK: true, Data: payload})
}

// injectContent 把流式重组的 content 注回 fs 参数（write action 的大载荷）。
func injectContent(args json.RawMessage, content string) json.RawMessage {
	var m map[string]any
	if err := json.Unmarshal(args, &m); err != nil {
		return args
	}
	m["content"] = content
	out, err := json.Marshal(m)
	if err != nil {
		return args
	}
	return out
}

// execute 执行一个 fs 请求并回送结果（必要时流式）。
func (ch *fsChannel) execute(id string, args json.RawMessage) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res, err := ch.svc.cfg.RunFS(ctx, args)
	if err != nil {
		_ = ch.sendFrame(&fsFrame{
			ID: id, Op: "result", OK: false,
			Error: err.Error(), State: string(proto.StateOf(err)),
		})
		return
	}
	payload, err := json.Marshal(res)
	if err != nil {
		_ = ch.sendFrame(&fsFrame{ID: id, Op: "result", OK: false, Error: fmt.Sprintf("marshal result: %v", err)})
		return
	}
	if len(payload) <= frameInlineLimit {
		_ = ch.sendFrame(&fsFrame{ID: id, Op: "result", OK: true, Data: payload})
		return
	}
	if err := ch.sendFrame(&fsFrame{ID: id, Op: "result", OK: true, Stream: true, Size: len(payload)}); err != nil {
		return
	}
	for seq, off := 0, 0; off < len(payload); seq, off = seq+1, off+frameChunkSize {
		end := off + frameChunkSize
		if end > len(payload) {
			end = len(payload)
		}
		if err := ch.sendFrame(&fsFrame{ID: id, Op: "chunk", Seq: seq, Text: string(payload[off:end])}); err != nil {
			return
		}
	}
	_ = ch.sendFrame(&fsFrame{ID: id, Op: "end"})
}

// sendFrame 发送一个 JSON 帧（发送串行化 + 高水位 backpressure）。
func (ch *fsChannel) sendFrame(f *fsFrame) error {
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	ch.sendMu.Lock()
	defer ch.sendMu.Unlock()
	deadline := time.Now().Add(30 * time.Second)
	for ch.dc.BufferedAmount() > bufHighWater {
		if time.Now().After(deadline) {
			return fmt.Errorf("rtc: send backpressure timeout")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return ch.dc.SendText(string(data))
}
