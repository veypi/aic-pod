package rtc

import (
	"context"
	"github.com/veypi/aic-pod/protocol"

	"sync"
	"time"

	"github.com/pion/webrtc/v4"
)

type peer struct {
	toolsDC       *webrtc.DataChannel
	streams       map[string]*webrtc.DataChannel // 流转发通道（按标签至多一条）
	toolsSend     sync.Mutex
	toolsResponse sync.Mutex
	streamSends   map[string]*sync.Mutex // 流发送锁，per-label（单通道拥塞不株连其它流）
	streamSendsMu sync.Mutex
	s             *Service
	id            string
	pc            *webrtc.PeerConnection
	ctx           context.Context
	cancel        context.CancelFunc
	mu            sync.Mutex
	lease         *peerLease // 认证租约（auth.go：身份/connectionID/绑定指纹/leaseUntil）
	created       time.Time
	closed        bool
	requests      chan struct{}
}

func (p *peer) close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.cancel()
	lease := p.lease
	p.lease = nil // 关闭即清租约：renew/查询不再视本 peer 为已认证
	p.mu.Unlock()
	if lease != nil && p.s.cfg.Disconnect != nil {
		// 断连清理按连接ID（租约过期也要清——前台执行按 connectionID 归属）。
		p.s.cfg.Disconnect(protocol.Caller{Subject: lease.subject, ConnectionID: lease.connectionID, Origin: lease.sessionID})
	}
	_ = p.pc.Close()
}
func (p *peer) expire(now time.Time) {
	p.mu.Lock()
	lease := p.lease
	created := p.created
	var until time.Time // lease 字段全锁内拷贝（renew 续租会写 until）
	if lease != nil {
		until = lease.until
	}
	p.mu.Unlock()
	if lease == nil {
		// 未认证 peer：30 秒认证窗口。
		if now.Sub(created) > 30*time.Second {
			p.s.drop(p)
		}
		return
	}
	if !until.After(now) {
		p.s.drop(p)
	}
}
func (p *peer) channel(dc *webrtc.DataChannel) {
	if dc.Label() == protocol.RtcChannel {
		p.toolsChannel(dc)
		return
	}
	// 流转发通道：按标签现查注册表（运行期新增的流式服务即时生效）。
	if p.s.cfg.Streams != nil {
		if relay, ok := p.s.cfg.Streams(dc.Label()); ok {
			p.streamChannel(dc, relay)
			return
		}
	}
	_ = dc.Close()
}
// streamLock 返回该标签的发送锁（懒建）。原全局 streamSend 单锁下，一个
// 拥塞通道可把锁持有到 30s 超时、期间其它流通道全阻塞；per-label 后互不影响。
func (p *peer) streamLock(label string) *sync.Mutex {
	p.streamSendsMu.Lock()
	defer p.streamSendsMu.Unlock()
	if p.streamSends == nil {
		p.streamSends = map[string]*sync.Mutex{}
	}
	l := p.streamSends[label]
	if l == nil {
		l = &sync.Mutex{}
		p.streamSends[label] = l
	}
	return l
}

func (p *peer) sendRaw(ctx context.Context, dc *webrtc.DataChannel, raw []byte, text bool) error {
	if dc == nil {
		return protocol.Fail("unreachable", "Channel is unavailable")
	}
	lock := &p.toolsSend
	if dc.Label() != protocol.RtcChannel {
		lock = p.streamLock(dc.Label())
	}
	lock.Lock()
	defer lock.Unlock()
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for dc.BufferedAmount()+uint64(len(raw)) > 4<<20 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return protocol.Fail("overloaded", "Channel send timeout")
		case <-ticker.C:
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if dc.ReadyState() != webrtc.DataChannelStateOpen {
		return protocol.Fail("unreachable", "Channel closed")
	}
	if text {
		return dc.SendText(string(raw))
	}
	return dc.Send(raw)
}
