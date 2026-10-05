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

type ToolBackend interface {
	HandleTool(context.Context, protocol.Caller, protocol.Request) protocol.Response
	DisconnectTools(protocol.Caller)
}

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

func (p *peer) toolCaller() (protocol.Caller, error) {
	p.mu.Lock()
	connection := p.connection
	p.mu.Unlock()
	c, err := p.s.cfg.Authorization.Caller(connection)
	if err != nil {
		return protocol.Caller{}, err
	}
	return protocol.Caller{Direct: true, Expiry: func() time.Time {
		current, err := p.s.cfg.Authorization.Caller(connection)
		if err != nil {
			return time.Time{}
		}
		return current.ExpiresAt
	}, Subject: c.Subject, ConnectionID: c.ConnectionID, Origin: c.SessionID, ExpiresAt: c.ExpiresAt, Check: func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := p.s.cfg.Authorization.Caller(connection)
		return err
	}}, nil
}
func (p *peer) toolsChannel(dc *webrtc.DataChannel) {
	if p.s.cfg.Tools == nil || !dc.Ordered() || dc.MaxPacketLifeTime() != nil || dc.MaxRetransmits() != nil {
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
			response := p.s.cfg.Tools.HandleTool(p.ctx, caller, *r.Tool)
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
			p.mu.Lock()
			if p.closed || p.connection != "" {
				err = protocol.Fail("unauthorized", "Peer already authenticated or closed")
			} else {
				admission, e := p.s.cfg.Authorization.Admit(r.Ticket, p.id, fp)
				err = e
				if e == nil {
					p.connection = admission.Caller.ConnectionID
					value = map[string]any{"host_id": p.s.cfg.HostID, "connection_id": p.connection, "expires_at": admission.Caller.ExpiresAt.UnixMilli()}
				}
			}
			p.mu.Unlock()
		case "renew":
			caller, e := p.toolCaller()
			err = e
			if e == nil {
				_, err = p.s.cfg.Authorization.Renew(caller.ConnectionID, r.Ticket, p.id, fp)
				value = map[string]bool{"renewed": err == nil}
			}
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
