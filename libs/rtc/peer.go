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
	browserDC     *webrtc.DataChannel
	toolsSend     sync.Mutex
	toolsResponse sync.Mutex
	browserSend   sync.Mutex
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
	p.mu.Unlock()
	if lease == nil {
		// 未认证 peer：30 秒认证窗口。
		if now.Sub(created) > 30*time.Second {
			p.s.drop(p)
		}
		return
	}
	if !lease.until.After(now) {
		p.s.drop(p)
	}
}
func (p *peer) channel(dc *webrtc.DataChannel) {
	if dc.Label() == protocol.RtcBrowserChannel {
		p.browserChannel(dc)
		return
	}
	if dc.Label() == protocol.RtcChannel {
		p.toolsChannel(dc)
		return
	}
	_ = dc.Close()
}
func (p *peer) sendRaw(ctx context.Context, dc *webrtc.DataChannel, raw []byte, text bool) error {
	if dc == nil {
		return protocol.Fail("unreachable", "Channel is unavailable")
	}
	lock := &p.toolsSend
	if dc.Label() == protocol.RtcBrowserChannel {
		lock = &p.browserSend
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
