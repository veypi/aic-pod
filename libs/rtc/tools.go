package rtc

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"github.com/veypi/aic-pod/protocol"
	"time"

	"github.com/pion/webrtc/v4"
)

func (p *peer) sendToolResponse(dc *webrtc.DataChannel, raw []byte) error {
	if len(raw) == 0 || len(raw) > protocol.RtcToolResponseLimit {
		return protocol.Fail("overloaded", "Device response too large")
	}
	// Keep every binary response contiguous even when tool calls finish together.
	// Authentication text may interleave; it never participates in reassembly.
	p.toolsResponse.Lock()
	defer p.toolsResponse.Unlock()
	if len(raw) <= protocol.RtcToolResponseChunkSize {
		return p.sendRaw(p.ctx, dc, raw, true)
	}
	first := make([]byte, protocol.RtcToolResponseChunkSize)
	binary.BigEndian.PutUint32(first, uint32(len(raw)))
	n := copy(first[4:], raw)
	if err := p.sendRaw(p.ctx, dc, first[:4+n], false); err != nil {
		return err
	}
	for raw = raw[n:]; len(raw) > 0; {
		n = min(len(raw), protocol.RtcToolResponseChunkSize)
		if err := p.sendRaw(p.ctx, dc, raw[:n], false); err != nil {
			return err
		}
		raw = raw[n:]
	}
	return nil
}

// toolCaller 由 peer 当前租约构造可信调用上下文：Expiry/Check 动态读
// 租约（含续租后期限），不是第一次认证的快照。
func (p *peer) toolCaller() (protocol.Caller, error) {
	lease := p.currentLease()
	if lease == nil {
		return protocol.Caller{}, protocol.Fail("unauthorized", "Connection authorization expired")
	}
	return protocol.Caller{Direct: true, Expiry: func() time.Time {
		if current := p.currentLease(); current != nil {
			return current.until
		}
		return time.Time{}
	}, Subject: lease.subject, ConnectionID: lease.connectionID, Origin: lease.sessionID, ExpiresAt: lease.until, Check: func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if p.currentLease() == nil {
			return protocol.Fail("unauthorized", "Connection authorization expired")
		}
		return nil
	}}, nil
}
func (p *peer) toolsChannel(dc *webrtc.DataChannel) {
	if !dc.Ordered() || dc.MaxPacketLifeTime() != nil || dc.MaxRetransmits() != nil {
		_ = dc.Close()
		return
	}
	p.mu.Lock()
	if p.toolsDC != nil {
		p.mu.Unlock()
		_ = dc.Close()
		return
	}
	p.toolsDC = dc
	p.mu.Unlock()
	dc.OnClose(func() { p.s.drop(p) })
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		var r protocol.RtcRequest
		if !msg.IsString || len(msg.Data) > protocol.MaxMessageBytes || protocol.Decode(msg.Data, &r) != nil || r.Validate() != nil {
			p.s.drop(p)
			return
		}
		if r.Auth != "" {
			go p.authenticate(dc, r)
			return
		}
		send := func(raw []byte) {
			if err := p.sendToolResponse(dc, raw); err != nil {
				// A partially delivered response cannot be resumed. Closing rejects
				// pending client calls immediately instead of leaving them to time out.
				p.s.drop(p)
			}
		}
		fail := func(err error) {
			raw, _ := json.Marshal(protocol.Reply(protocol.RtcProtocol, r.Tool.ID, nil, err))
			send(raw)
		}
		caller, err := p.toolCaller()
		if err != nil {
			fail(err)
			return
		}
		caller.GrantApproved = r.GrantApproved
		invoke := func() {
			response := p.s.cfg.Dispatch(p.ctx, caller, *r.Tool)
			raw, err := json.Marshal(response)
			if err != nil {
				fail(err)
				return
			}
			if len(raw) > protocol.RtcToolResponseLimit {
				// Keep only small execution metadata so callers can locate the
				// completed output without replaying a possibly mutating command.
				var result any
				if exec, ok := response.Result.(*protocol.Output); ok && exec != nil {
					attrs := make(map[string]string)
					for _, key := range []string{"action", "exit_code", "output", "error_output"} {
						if value := exec.Attrs[key]; value != "" && len(value) <= 4096 {
							attrs[key] = value
						}
					}
					result = &protocol.Output{Attrs: attrs}
				}
				err := protocol.Fail("overloaded", fmt.Sprintf("Device response is %d bytes, exceeding the %d MiB limit; use execution logs for the complete output", len(raw), protocol.RtcToolResponseLimit>>20))
				raw, _ = json.Marshal(protocol.Reply(protocol.RtcProtocol, r.Tool.ID, result, err))
				send(raw)
				return
			}
			send(raw)
		}
		// Cancellation must not queue behind the work it cancels.
		if r.Tool.Action == protocol.ActionCancel {
			invoke()
			return
		}
		select {
		case p.requests <- struct{}{}:
			go func() { defer func() { <-p.requests }(); invoke() }()
		default:
			fail(protocol.Fail("overloaded", "Too many pending requests"))
		}
	})
}
func (p *peer) authenticate(dc *webrtc.DataChannel, r protocol.RtcRequest) {
	var value any
	fp, err := p.fingerprint()
	if err == nil {
		switch r.Auth {
		case "open":
			value, err = p.admit(r.Ticket, fp)
		case "renew":
			var renewed map[string]bool
			renewed, err = p.renew(r.Ticket, fp)
			value = renewed
		default:
			err = protocol.Fail("invalid_argument", "Unknown authentication operation")
		}
	}
	result := protocol.RtcAuthResult{ID: r.ID, Result: value}
	if err != nil {
		result.Error = protocol.AsFault(err)
	}
	raw, _ := json.Marshal(result)
	if err = p.sendRaw(p.ctx, dc, raw, true); err != nil {
		p.s.drop(p)
	}
}
