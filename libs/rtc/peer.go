package rtc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	connection    string
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
	connection := p.connection
	p.mu.Unlock()
	if connection != "" {
		if p.s.cfg.Tools != nil {
			if caller, err := p.toolCaller(); err == nil {
				p.s.cfg.Tools.DisconnectTools(caller)
			}
		}
		p.s.cfg.Authorization.Close(connection)
	}
	_ = p.pc.Close()
}
func (p *peer) expire(now time.Time) {
	p.mu.Lock()
	conn := p.connection
	created := p.created
	p.mu.Unlock()
	if conn == "" {
		if now.Sub(created) > 30*time.Second {
			p.s.drop(p)
		}
		return
	}
	if _, err := p.s.cfg.Authorization.Caller(conn); err != nil {
		p.s.drop(p)
		return
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
func (p *peer) fingerprint() (string, error) {
	if p.pc.SCTP() == nil || p.pc.SCTP().Transport() == nil {
		return "", protocol.Fail("unauthorized", "DTLS is unavailable")
	}
	cert := p.pc.SCTP().Transport().GetRemoteCertificate()
	if len(cert) == 0 {
		return "", protocol.Fail("unauthorized", "Peer certificate unavailable")
	}
	sum := sha256.Sum256(cert)
	return protocol.NormalizeFingerprint("sha-256 " + hex.EncodeToString(sum[:]))
}
