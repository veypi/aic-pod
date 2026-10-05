package rtc

import (
	"context"
	"encoding/binary"
	"sync"

	"github.com/pion/webrtc/v4"
	rtcwire "github.com/veypi/aic-pod/protocol/hosts_rtc"
	wire "github.com/veypi/aic-pod/protocol/tool"
)

// BrowserRelay transports upstream WebSocket messages, not browser commands.
type BrowserRelay func(context.Context, wire.Caller, <-chan []byte, func([]byte) error) error

func (p *peer) browserChannel(dc *webrtc.DataChannel) {
	if p.s.cfg.Browser == nil || !dc.Ordered() || dc.MaxPacketLifeTime() != nil || dc.MaxRetransmits() != nil {
		_ = dc.Close()
		return
	}
	caller, err := p.toolCaller()
	if err != nil {
		_ = dc.Close()
		return
	}
	p.mu.Lock()
	if p.browserDC != nil {
		p.mu.Unlock()
		_ = dc.Close()
		return
	}
	p.browserDC = dc
	p.mu.Unlock()
	ctx, cancel := context.WithCancel(p.ctx)
	input := make(chan []byte, 128)
	var once sync.Once
	closeStream := func() {
		once.Do(func() {
			cancel()
			_ = dc.Close()
			p.mu.Lock()
			if p.browserDC == dc {
				p.browserDC = nil
			}
			p.mu.Unlock()
		})
	}
	dc.OnClose(closeStream)
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		if !msg.IsString || len(msg.Data) > 64<<10 {
			closeStream()
			return
		}
		select {
		case input <- append([]byte(nil), msg.Data...):
		default:
			closeStream()
		}
	})
	dc.OnOpen(func() {
		go func() {
			defer closeStream()
			send := func(raw []byte) error {
				if len(raw) == 0 || len(raw) > rtcwire.BrowserMessageLimit {
					return wire.Fail("overloaded", "Browser frame too large")
				}
				// One upstream message may exceed SCTP's message limit. Only transport
				// framing is added: uint32 length, followed by ordered 16 KiB chunks.
				data := make([]byte, 4+len(raw))
				binary.BigEndian.PutUint32(data, uint32(len(raw)))
				copy(data[4:], raw)
				for len(data) > 0 {
					n := min(len(data), 16<<10)
					if err := p.sendRaw(ctx, dc, data[:n], false); err != nil {
						return err
					}
					data = data[n:]
				}
				return nil
			}
			if err := p.s.cfg.Browser(ctx, caller, input, send); err != nil && ctx.Err() == nil {
				_ = p.sendRaw(ctx, dc, []byte(err.Error()), true)
			}
		}()
	})
}
