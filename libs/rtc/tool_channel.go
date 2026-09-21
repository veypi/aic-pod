package rtc

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/pion/webrtc/v4"
	tool "github.com/veypi/aic-pod/libs/hosts_tool"
	rtcwire "github.com/veypi/aic-pod/protocol/hosts_rtc"
	wire "github.com/veypi/aic-pod/protocol/hosts_tools"
	"io"
	"strings"
	"sync"
	"time"
)

// An attachment only routes bytes. No per-message request, response, sequence,
// schema, tool event, or direction is interpreted here.
type toolChannel struct {
	p        *peer
	dc       *webrtc.DataChannel
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	source   tool.Stream
	bound    bool
	incoming chan []byte
	once     sync.Once
}

func (p *peer) toolChannel(dc *webrtc.DataChannel) {
	label := dc.Label()
	if p.s.cfg.Tools == nil || !wire.ValidID(strings.TrimPrefix(label, rtcwire.StreamPrefix)) {
		dc.Close()
		return
	}
	if _, err := p.toolCaller(); err != nil {
		dc.Close()
		return
	}
	ctx, cancel := context.WithCancel(p.ctx)
	s := &toolChannel{p: p, dc: dc, ctx: ctx, cancel: cancel, incoming: make(chan []byte, 128)}
	p.mu.Lock()
	if p.closed || len(p.toolStreams) >= 32 || p.toolStreams[label] != nil {
		p.mu.Unlock()
		cancel()
		dc.Close()
		return
	}
	if p.toolStreams == nil {
		p.toolStreams = map[string]*toolChannel{}
	}
	p.toolStreams[label] = s
	p.mu.Unlock()
	dc.OnClose(func() { s.close(nil) })
	dc.OnError(func(err error) { s.close(err) })
	dc.OnMessage(func(m webrtc.DataChannelMessage) {
		s.mu.Lock()
		bound := s.bound
		s.mu.Unlock()
		if !bound || m.IsString || len(m.Data) > rtcwire.MaxDatagramBytes {
			s.close(wire.Fail("invalid_message", "Expected a bounded binary channel message"))
			return
		}
		select {
		case <-ctx.Done():
		case s.incoming <- append([]byte(nil), m.Data...):
		default:
			s.close(wire.Fail("overloaded", "Channel receive buffer full"))
		}
	})
	go func() {
		timer := time.NewTimer(10 * time.Second)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		s.mu.Lock()
		bound := s.bound
		s.mu.Unlock()
		if !bound {
			s.close(wire.Fail("deadline_exceeded", "Channel was not bound"))
		}
	}()
}
func (p *peer) openToolChannel(c tool.Caller, r rtcwire.Request) (any, error) {
	probe := r.Request
	probe.Action = "call"
	if err := probe.Validate(); err != nil {
		return nil, err
	}
	if r.Call == nil || len(r.Argv) != 0 || r.TimeoutMS != 0 {
		return nil, wire.Fail("invalid_argument", "Expected tool, method and opening args")
	}
	p.mu.Lock()
	s := p.toolStreams[r.Channel]
	p.mu.Unlock()
	if s == nil {
		return nil, wire.Fail("not_found", "RTC channel not found on this connection")
	}
	s.mu.Lock()
	if s.bound || s.ctx.Err() != nil {
		s.mu.Unlock()
		return nil, wire.Fail("busy", "Channel already bound or closed")
	}
	s.bound = true
	s.mu.Unlock()
	source, err := p.s.cfg.Tools.OpenToolStream(s.ctx, c, *r.Call)
	if err != nil {
		s.close(err)
		return nil, err
	}
	s.mu.Lock()
	if s.ctx.Err() != nil {
		s.mu.Unlock()
		source.Close()
		return nil, wire.Fail("closed", "Channel closed while opening")
	}
	s.source = source
	s.mu.Unlock()
	go s.receive(source)
	go s.transmit(source)
	return map[string]any{"channel": r.Channel, "max_message_bytes": rtcwire.MaxDatagramBytes}, nil
}
func (s *toolChannel) receive(source tool.Stream) {
	for {
		select {
		case <-s.ctx.Done():
			return
		case b := <-s.incoming:
			if err := source.Send(s.ctx, b); err != nil {
				s.close(err)
				return
			}
		}
	}
}
func (s *toolChannel) transmit(source tool.Stream) {
	for {
		b, err := source.Recv(s.ctx)
		if err != nil {
			s.close(err)
			return
		}
		if len(b) > rtcwire.MaxDatagramBytes {
			s.close(wire.Fail("output_limit", "Channel message too large"))
			return
		}
		// Wait only for the local SCTP buffer, never a remote application ACK.
		for s.dc.BufferedAmount()+uint64(len(b)) > 256<<10 {
			select {
			case <-s.ctx.Done():
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
		if s.ctx.Err() != nil {
			return
		}
		if err := s.dc.Send(b); err != nil {
			s.close(err)
			return
		}
	}
}
func (s *toolChannel) close(err error) {
	s.once.Do(func() {
		s.cancel()
		s.mu.Lock()
		source := s.source
		s.source = nil
		s.mu.Unlock()
		s.p.mu.Lock()
		delete(s.p.toolStreams, s.dc.Label())
		control := s.p.toolsDC
		s.p.mu.Unlock()
		_ = s.dc.Close()
		// Native cleanup must not block another channel's receive callback.
		go func() {
			if source != nil {
				_ = source.Close()
			}
			if control == nil || s.p.ctx.Err() != nil {
				return
			}
			note := rtcwire.Closed{Protocol: rtcwire.Protocol, Event: "stream.closed", Channel: s.dc.Label()}
			if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) {
				note.Error = wire.AsFault(err)
			}
			b, _ := json.Marshal(note)
			_ = s.p.sendRaw(s.p.ctx, control, b, true)
		}()
	})
}
