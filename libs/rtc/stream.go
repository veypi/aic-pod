package rtc

import (
	"context"
	"encoding/binary"
	"github.com/veypi/aic-pod/protocol"
	"sync"

	"github.com/pion/webrtc/v4"
)

// StreamRelay transports upstream stream messages (e.g. the browser daemon's
// native WebSocket), not tool calls. 消息边界保持的纯转发；权限复查与保活由
// 实现方负责。
type StreamRelay func(context.Context, protocol.Caller, <-chan []byte, func([]byte) error) error

// streamChannel 是流转发通道的通用实现（原 browserChannel 泛化）：同一 peer
// 上每个标签最多一条；帧格式 = uint32 大端长度 + 有序 16 KiB 分块。
func (p *peer) streamChannel(dc *webrtc.DataChannel, relay StreamRelay) {
	if !dc.Ordered() || dc.MaxPacketLifeTime() != nil || dc.MaxRetransmits() != nil {
		_ = dc.Close()
		return
	}
	caller, err := p.toolCaller()
	if err != nil {
		_ = dc.Close()
		return
	}
	label := dc.Label()
	p.mu.Lock()
	if p.streams[label] != nil {
		p.mu.Unlock()
		_ = dc.Close()
		return
	}
	p.streams[label] = dc
	p.mu.Unlock()
	ctx, cancel := context.WithCancel(p.ctx)
	input := make(chan []byte, 128)
	var once sync.Once
	closeStream := func() {
		once.Do(func() {
			cancel()
			_ = dc.Close()
			p.mu.Lock()
			if p.streams[label] == dc {
				delete(p.streams, label)
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
				if len(raw) == 0 || len(raw) > protocol.RtcStreamMessageLimit {
					return protocol.Fail("overloaded", "Stream frame too large")
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
			if err := relay(ctx, caller, input, send); err != nil && ctx.Err() == nil {
				_ = p.sendRaw(ctx, dc, []byte(err.Error()), true)
			}
		}()
	})
}
