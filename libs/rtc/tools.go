package rtc

import (
	"context"
	"encoding/json"
	"github.com/pion/webrtc/v4"
	tool "github.com/veypi/aic-pod/libs/hosts_tool"
	rtcwire "github.com/veypi/aic-pod/protocol/hosts_rtc"
	wire "github.com/veypi/aic-pod/protocol/hosts_tools"
	"time"
)

type ToolBackend interface {
	HandleTool(context.Context, tool.Caller, wire.Request) wire.Response
	DisconnectTools(tool.Caller)
	OpenToolStream(context.Context, tool.Caller, wire.Invocation) (tool.Stream, error)
}

func (p *peer) toolCaller() (tool.Caller, error) {
	p.mu.Lock()
	connection := p.connection
	p.mu.Unlock()
	c, err := p.s.cfg.Authorization.Caller(connection)
	if err != nil {
		return tool.Caller{}, err
	}
	return tool.Caller{AllowStreams: true, Expiry: func() time.Time {
		current, err := p.s.cfg.Authorization.Caller(connection)
		if err != nil {
			return time.Time{}
		}
		return current.ExpiresAt
	}, Subject: c.Subject, ConnectionID: c.ConnectionID, Level: 9, ExpiresAt: c.ExpiresAt, Check: func(ctx context.Context) error {
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
		var req rtcwire.Request
		err := wire.Decode(msg.Data, &req)
		if !msg.IsString || len(msg.Data) > wire.MaxMessageBytes || err != nil || req.Protocol != rtcwire.Protocol || !wire.ValidID(req.ID) {
			_ = p.sendTool(dc, wire.Reply(rtcwire.Protocol, req.ID, nil, wire.Fail("invalid_argument", "Expected hosts_rtc/1 request")))
			return
		}
		// Call cancellation must remain reachable when requests are waiting.
		if req.Action == "call.cancel" {
			p.toolRequest(dc, req)
			return
		}
		select {
		case p.requests <- struct{}{}:
			go func() { defer func() { <-p.requests }(); p.toolRequest(dc, req) }()
		default:
			_ = p.sendTool(dc, wire.Reply(rtcwire.Protocol, req.ID, nil, wire.Fail("overloaded", "Too many pending requests")))
		}
	})
}
func (p *peer) sendTool(dc *webrtc.DataChannel, r wire.Response) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	limit := wire.MaxMessageBytes
	if transport := dc.Transport(); transport != nil {
		if peerLimit := int(transport.GetCapabilities().MaxMessageSize); peerLimit > 0 && peerLimit < limit {
			limit = peerLimit
		}
	}
	if len(raw) > limit {
		raw, _ = json.Marshal(wire.Reply(rtcwire.Protocol, r.ID, nil, wire.Fail("output_limit", "Response too large; reduce observation limit or result page size")))
	}
	return p.sendRaw(p.ctx, dc, raw, true)
}
func (p *peer) toolRequest(dc *webrtc.DataChannel, r rtcwire.Request) {
	var response wire.Response
	if r.Action == "hello" {
		fp, err := p.fingerprint()
		var value any
		if err == nil {
			p.mu.Lock()
			if p.closed || p.connection != "" {
				err = wire.Fail("unauthorized", "Peer already authenticated or closed")
			} else {
				admission, e := p.s.cfg.Authorization.Admit(r.Ticket, p.id, fp)
				err = e
				if err == nil {
					p.connection = admission.Caller.ConnectionID
					value = map[string]any{"host_id": p.s.cfg.HostID, "protocol": rtcwire.Protocol, "tools_protocol": wire.Protocol, "connection_id": p.connection, "expires_at": admission.Caller.ExpiresAt.UnixMilli()}
				}
			}
			p.mu.Unlock()
		}
		response = wire.Reply(rtcwire.Protocol, r.ID, value, err)
	} else {
		caller, err := p.toolCaller()
		if err != nil {
			response = wire.Reply(rtcwire.Protocol, r.ID, nil, err)
		} else if r.Action == "auth.renew" {
			fp, e := p.fingerprint()
			if e == nil {
				_, e = p.s.cfg.Authorization.Renew(caller.ConnectionID, r.Ticket, p.id, fp)
			}
			response = wire.Reply(rtcwire.Protocol, r.ID, map[string]bool{"renewed": e == nil}, e)
		} else if r.Action == "stream.open" {
			value, err := p.openToolChannel(caller, r)
			response = wire.Reply(rtcwire.Protocol, r.ID, value, err)
		} else {
			response = p.s.cfg.Tools.HandleTool(p.ctx, caller, r.Request)
		}
	}
	if err := p.sendTool(dc, response); err != nil {
		p.s.drop(p)
	}
}
