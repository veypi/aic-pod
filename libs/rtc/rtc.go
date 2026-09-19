// Package rtc transports hosts/1 over authenticated, reliable WebRTC channels.
// It has no filesystem or UI command dispatch: all business calls enter Backend.
package rtc

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
	"github.com/veypi/aic-pod/libs/hostcmd"
	"github.com/veypi/aic-pod/libs/proto"
	"github.com/veypi/aic-pod/protocol/hosts"
)

const maxPeerConnections = 16
const controlLabel = "hosts-control"
const dataLabel = "hosts-data"
const liveLabel = "hosts-live"

type Backend interface {
	Authorization() *hostcmd.Access
	Epoch() string
	HandlePacket(context.Context, string, hosts.Packet) hosts.Packet
	Limits(bool) map[string]any
	CheckSession(string, string) error
	Disconnect(string)
	Reap()
}
type Config struct {
	HostID, Hostname, Version string
	Send                      func(*proto.RtcSignal)
	Commands                  Backend
	Logf                      func(string, ...any)
}
type Service struct {
	cfg    Config
	api    *webrtc.API
	conn   *net.UDPConn
	mu     sync.Mutex
	pcs    map[string]*peer
	closed bool
	done   chan struct{}
	logf   func(string, ...any)
}

func New(cfg Config) (*Service, error) {
	if cfg.Commands == nil || cfg.Send == nil || !hosts.ValidID(cfg.HostID) {
		return nil, fmt.Errorf("rtc: Commands, Send and HostID are required")
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: 0})
	if err != nil {
		return nil, fmt.Errorf("rtc: udp listen: %w", err)
	}
	se := webrtc.SettingEngine{}
	se.SetICEUDPMux(ice.NewUDPMuxDefault(ice.UDPMuxParams{UDPConn: conn}))
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeQueryOnly)
	se.SetSCTPMaxMessageSize(hosts.MaxControlBytes)
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	s := &Service{cfg: cfg, api: webrtc.NewAPI(webrtc.WithSettingEngine(se)), conn: conn, pcs: map[string]*peer{}, done: make(chan struct{}), logf: cfg.Logf}
	go s.maintain()
	return s, nil
}
func (s *Service) maintain() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case now := <-ticker.C:
			s.mu.Lock()
			peers := make([]*peer, 0, len(s.pcs))
			for _, p := range s.pcs {
				peers = append(peers, p)
			}
			s.mu.Unlock()
			for _, p := range peers {
				p.expire(now)
			}
			s.cfg.Commands.Reap()
		}
	}
}
func (s *Service) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	close(s.done)
	peers := s.pcs
	s.pcs = map[string]*peer{}
	s.mu.Unlock()
	for _, p := range peers {
		p.close()
	}
	_ = s.conn.Close()
}
func (s *Service) drop(p *peer) {
	s.mu.Lock()
	if s.pcs[p.id] == p {
		delete(s.pcs, p.id)
	}
	s.mu.Unlock()
	p.close()
}
func (s *Service) HandleSignal(sig *proto.RtcSignal) {
	if sig == nil || !hosts.ValidID(sig.PC) {
		return
	}
	if sig.Kind == proto.RtcOffer {
		s.offer(sig)
		return
	}
	s.mu.Lock()
	p := s.pcs[sig.PC]
	s.mu.Unlock()
	if p == nil {
		return
	}
	switch sig.Kind {
	case proto.RtcBye:
		s.drop(p)
	case proto.RtcCandidate:
		if len(sig.Candidate) > 8192 {
			return
		}
		var c webrtc.ICECandidateInit
		if json.Unmarshal([]byte(sig.Candidate), &c) == nil {
			_ = p.pc.AddICECandidate(c)
		}
	}
}
func (s *Service) offer(sig *proto.RtcSignal) {
	if len(sig.SDP) == 0 || len(sig.SDP) > hosts.MaxControlBytes {
		return
	}
	s.mu.Lock()
	// Duplicate offers cannot replace an authenticated peer with the same ID.
	if s.closed || s.pcs[sig.PC] != nil || len(s.pcs) >= maxPeerConnections {
		s.mu.Unlock()
		return
	}
	pc, err := s.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &peer{s: s, id: sig.PC, pc: pc, ctx: ctx, cancel: cancel, created: time.Now(), requests: make(chan struct{}, 32)}
	s.pcs[sig.PC] = p
	s.mu.Unlock()
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			b, _ := json.Marshal(c.ToJSON())
			s.cfg.Send(&proto.RtcSignal{PC: p.id, Kind: proto.RtcCandidate, Candidate: string(b)})
		}
	})
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		if st == webrtc.PeerConnectionStateFailed || st == webrtc.PeerConnectionStateClosed || st == webrtc.PeerConnectionStateDisconnected {
			s.drop(p)
		}
	})
	pc.OnDataChannel(p.channel)
	if err = pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sig.SDP}); err != nil {
		s.drop(p)
		return
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		s.drop(p)
		return
	}
	if err = pc.SetLocalDescription(answer); err != nil {
		s.drop(p)
		return
	}
	s.cfg.Send(&proto.RtcSignal{PC: p.id, Kind: proto.RtcAnswer, SDP: pc.LocalDescription().SDP})
}
