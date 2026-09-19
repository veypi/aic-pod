package rtc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/veypi/aic-pod/protocol/hosts"
)

type peer struct {
	s                               *Service
	id                              string
	pc                              *webrtc.PeerConnection
	ctx                             context.Context
	cancel                          context.CancelFunc
	mu                              sync.Mutex
	control, data, live             *webrtc.DataChannel
	connection                      string
	created                         time.Time
	closed                          bool
	requests                        chan struct{}
	controlSend, dataSend, liveSend sync.Mutex
	lives                           map[string]*liveStream
	liveIDs                         map[string]bool
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
	lives := p.lives
	p.lives = nil
	p.mu.Unlock()
	for _, l := range lives {
		l.stop()
	}
	if connection != "" {
		p.s.cfg.Commands.Disconnect(connection)
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
	if _, err := p.s.cfg.Commands.Authorization().Caller(conn); err != nil {
		p.s.drop(p)
		return
	}

}
func (p *peer) channel(dc *webrtc.DataChannel) {
	if !dc.Ordered() || dc.MaxPacketLifeTime() != nil || dc.MaxRetransmits() != nil {
		_ = dc.Close()
		return
	}
	p.mu.Lock()
	switch dc.Label() {
	case controlLabel:
		if p.control != nil {
			p.mu.Unlock()
			_ = dc.Close()
			return
		}
		p.control = dc
	case liveLabel:
		if p.live != nil {
			p.mu.Unlock()
			_ = dc.Close()
			return
		}
		p.live = dc
	case dataLabel:
		if p.data != nil {
			p.mu.Unlock()
			_ = dc.Close()
			return
		}
		p.data = dc
	default:
		p.mu.Unlock()
		_ = dc.Close()
		return
	}
	p.mu.Unlock()
	dc.OnClose(func() { p.s.drop(p) })
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		if dc.Label() == liveLabel {
			p.s.drop(p)
			return
		}
		if dc.Label() == dataLabel && !msg.IsString {
			p.binary(msg.Data)
			return
		}
		if !msg.IsString || len(msg.Data) > hosts.MaxControlBytes {
			p.s.drop(p)
			return
		}
		var envelope struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(msg.Data, &envelope) != nil {
			p.s.drop(p)
			return
		}
		if envelope.Type == "event" && dc.Label() == controlLabel {
			p.event(msg.Data)
			return
		}
		req, err := hosts.ParseRequest(msg.Data)
		if err != nil {
			_ = p.send(dc, hosts.Reply(req.ID, nil, err))
			return
		}
		select {
		case p.requests <- struct{}{}:
		default:
			_ = p.send(dc, hosts.Reply(req.ID, nil, hosts.Fail("overloaded", "Too many pending requests")))
			return
		}
		raw := append([]byte(nil), msg.Data...)
		go func() { defer func() { <-p.requests }(); p.request(dc, req, raw) }()
	})
}
func (p *peer) send(dc *webrtc.DataChannel, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(raw) > hosts.MaxControlBytes {
		return hosts.Fail("overloaded", "Control response requires a byte source")
	}
	return p.sendRaw(p.ctx, dc, raw, true)
}
func (p *peer) sendRaw(ctx context.Context, dc *webrtc.DataChannel, raw []byte, text bool) error {
	if dc == nil {
		return hosts.Fail("unreachable", "Channel is unavailable")
	}
	lock := &p.dataSend
	if dc.Label() == liveLabel {
		lock = &p.liveSend
	}
	if text && dc.Label() == controlLabel {
		lock = &p.controlSend
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
			return hosts.Fail("overloaded", "Channel send timeout")
		case <-ticker.C:
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if dc.ReadyState() != webrtc.DataChannelStateOpen {
		return hosts.Fail("unreachable", "Channel closed")
	}
	if text {
		return dc.SendText(string(raw))
	}
	return dc.Send(raw)
}
func (p *peer) emit(name string, value any) {
	raw, _ := json.Marshal(value)
	p.mu.Lock()
	dc := p.control
	p.mu.Unlock()
	if err := p.send(dc, hosts.Event{V: 1, Type: "event", Event: name, Data: raw}); err != nil {
		p.s.drop(p)
	}
}
func (p *peer) fingerprint() (string, error) {
	if p.pc.SCTP() == nil || p.pc.SCTP().Transport() == nil {
		return "", hosts.Fail("unauthorized", "DTLS is unavailable")
	}
	cert := p.pc.SCTP().Transport().GetRemoteCertificate()
	if len(cert) == 0 {
		return "", hosts.Fail("unauthorized", "Peer certificate unavailable")
	}
	sum := sha256.Sum256(cert)
	return hosts.NormalizeFingerprint("sha-256 " + hex.EncodeToString(sum[:]))
}
func (p *peer) request(dc *webrtc.DataChannel, req hosts.Request, raw []byte) {
	ctx, cancel := context.WithTimeout(p.ctx, 30*time.Second)
	defer cancel()
	var value any
	var err error
	var after func()
	p.mu.Lock()
	conn := p.connection
	p.mu.Unlock()
	switch req.Method {
	case "hello":
		var args struct {
			Protocol string `json:"protocol"`
			Ticket   string `json:"ticket"`
		}
		err = hosts.Decode(req.Params, &args)
		if err == nil && (dc.Label() != controlLabel || args.Protocol != hosts.Protocol) {
			err = hosts.Fail("unsupported_protocol", "Expected hosts/1 control channel")
		}
		if err == nil {
			var fp string
			fp, err = p.fingerprint()
			if err == nil {
				p.mu.Lock()
				if p.closed || p.connection != "" {
					err = hosts.Fail("unauthorized", "Peer already authenticated or closed")
				} else {
					admission, e := p.s.cfg.Commands.Authorization().Admit(args.Ticket, p.id, fp)
					err = e
					if err == nil {
						p.connection = admission.Caller.ConnectionID
						value = map[string]any{"protocol": hosts.Protocol, "host_id": p.s.cfg.HostID, "connection_id": p.connection, "runtime_epoch": p.s.cfg.Commands.Epoch(), "data_token": admission.DataToken, "lease_until": admission.Caller.ExpiresAt.Unix(), "limits": p.s.cfg.Commands.Limits(false)}
					}
				}
				p.mu.Unlock()
			}
		}
	case "data.bind":
		var args struct {
			Connection string `json:"connection_id"`
			Epoch      string `json:"runtime_epoch"`
			Token      string `json:"token"`
		}
		err = hosts.Decode(req.Params, &args)
		if err == nil && (dc.Label() != dataLabel || args.Connection != conn || args.Epoch != p.s.cfg.Commands.Epoch()) {
			err = hosts.Fail("unauthorized", "Invalid data binding")
		}
		if err == nil {
			err = p.s.cfg.Commands.Authorization().Bind(conn, p.id, args.Token)
			value = map[string]bool{"bound": err == nil}
		}
	default:
		if dc.Label() != controlLabel {
			err = hosts.Fail("unsupported", "Business requests require the control channel")
			break
		}
		if _, err = p.s.cfg.Commands.Authorization().Caller(conn); err != nil {
			break
		}
		switch req.Method {
		case "live.open":
			value, after, err = p.openLive(conn, req.Params)
		case "live.close":
			var args struct {
				Session string `json:"session_id"`
				Stream  string `json:"stream_id"`
			}
			err = hosts.Decode(req.Params, &args)
			if err == nil {
				err = p.s.cfg.Commands.CheckSession(conn, args.Session)
			}
			if err == nil {
				p.mu.Lock()
				l := p.lives[args.Stream]
				p.mu.Unlock()
				if l != nil && l.session != args.Session {
					err = hosts.Fail("expired", "Live stream belongs to another session")
				} else {
					if l != nil {
						p.endLive(l, nil)
					}
					value = map[string]bool{"closed": true}
				}
			}
		case "auth.renew":
			var args struct {
				Ticket string `json:"ticket"`
			}
			err = hosts.Decode(req.Params, &args)
			if err == nil {
				var fp string
				fp, err = p.fingerprint()
				if err == nil {
					caller, e := p.s.cfg.Commands.Authorization().Renew(conn, args.Ticket, p.id, fp)
					err = e
					value = map[string]any{"lease_until": caller.ExpiresAt.Unix()}
				}
			}
		default:
			response := p.s.cfg.Commands.HandlePacket(ctx, conn, hosts.Packet{Data: raw})
			target := dc
			if response.Binary {
				p.mu.Lock()
				target = p.data
				p.mu.Unlock()
			}
			if e := p.sendRaw(ctx, target, response.Data, !response.Binary); e != nil {
				p.s.drop(p)
			}
			return
		}
	}
	response := hosts.Reply(req.ID, value, err)
	if e := p.send(dc, response); e != nil {
		p.s.drop(p)
		return
	}
	if err == nil && after != nil {
		after()
	}
}
func (p *peer) event(raw []byte) {
	var e hosts.Event
	if hosts.Decode(raw, &e) != nil || e.V != 1 || e.Type != "event" {
		p.s.drop(p)
		return
	}
	p.mu.Lock()
	conn := p.connection
	p.mu.Unlock()
	if _, err := p.s.cfg.Commands.Authorization().DataCaller(conn); err != nil {
		p.s.drop(p)
		return
	}
	switch e.Event {
	case "live.input", "live.ack":
		p.liveEvent(e)

	default:
		p.s.drop(p)
	}
}
func (p *peer) binary(raw []byte) {
	p.mu.Lock()
	conn, dc := p.connection, p.control
	p.mu.Unlock()
	response := p.s.cfg.Commands.HandlePacket(p.ctx, conn, hosts.Packet{Binary: true, Data: raw})
	if err := p.sendRaw(p.ctx, dc, response.Data, true); err != nil {
		p.s.drop(p)
	}
}
