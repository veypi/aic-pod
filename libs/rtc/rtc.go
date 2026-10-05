// Package rtc transports hosts_rtc/3 over authenticated, reliable WebRTC channels.
// It has no filesystem or UI command dispatch: all business calls enter Backend.
package rtc

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/veypi/aic-pod/protocol"
	"net"
	"sync"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
)

const maxPeerConnections = 16

type Config struct {
	HostID, Hostname, Version string
	// 票据验签材料（rtc direct key 派生自设备凭据）。
	UserID            string
	CredentialVersion uint64
	Key               []byte
	Now               func() time.Time // 可选时钟（测试注入）
	Send              func(*protocol.RtcSignal)
	// Dispatch/Disconnect 直接接业务分发与断连清理的方法值
	// （host：HandleTool/DisconnectTools——不再有 ToolBackend 转发层）。
	Dispatch   func(context.Context, protocol.Caller, protocol.Request) protocol.Response
	Disconnect func(protocol.Caller)
	Browser    BrowserRelay
	Logf       func(string, ...any)
}
type Service struct {
	cfg      Config
	api      *webrtc.API
	conn     *net.UDPConn
	mu       sync.Mutex
	pcs      map[string]*peer
	consumed map[string]time.Time // 一次性 ticket 消费缓存（ticket ID → 准入过期）
	closed   bool
	done     chan struct{}
	logf     func(string, ...any)
}

func New(cfg Config) (*Service, error) {
	if !protocol.ValidID(cfg.HostID) || !protocol.ValidID(cfg.UserID) || cfg.CredentialVersion == 0 || len(cfg.Key) != 32 {
		return nil, fmt.Errorf("rtc: HostID, UserID, CredentialVersion and a 32-byte Key are required")
	}
	if cfg.Dispatch == nil || cfg.Disconnect == nil || cfg.Send == nil {
		return nil, fmt.Errorf("rtc: Dispatch, Disconnect and Send are required")
	}
	cfg.Key = append([]byte(nil), cfg.Key...)
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: 0})
	if err != nil {
		return nil, fmt.Errorf("rtc: udp listen: %w", err)
	}
	se := webrtc.SettingEngine{}
	se.SetICEUDPMux(ice.NewUDPMuxDefault(ice.UDPMuxParams{UDPConn: conn}))
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeQueryOnly)
	se.SetSCTPMaxMessageSize(1 << 20)
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	s := &Service{cfg: cfg, api: webrtc.NewAPI(webrtc.WithSettingEngine(se)), conn: conn, pcs: map[string]*peer{}, consumed: map[string]time.Time{}, done: make(chan struct{}), logf: cfg.Logf}
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
		case <-ticker.C:
			// 时效判定统一走 s.now()（host 装配的是平台校准时钟）；
			// ticker 的原始时间只在时钟准确时与之一致。
			now := s.now()
			s.mu.Lock()
			// 清扫一次性 ticket 消费缓存（按准入过期时间）。
			for id, until := range s.consumed {
				if !until.After(now) {
					delete(s.consumed, id)
				}
			}
			peers := make([]*peer, 0, len(s.pcs))
			for _, p := range s.pcs {
				peers = append(peers, p)
			}
			s.mu.Unlock()
			for _, p := range peers {
				p.expire(now)
			}
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
func (s *Service) HandleSignal(sig *protocol.RtcSignal) {
	if sig == nil || !protocol.ValidID(sig.PC) {
		return
	}
	if sig.Kind == protocol.RtcOffer {
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
	case protocol.RtcBye:
		s.drop(p)
	case protocol.RtcCandidate:
		if len(sig.Candidate) > 8192 {
			return
		}
		var c webrtc.ICECandidateInit
		if json.Unmarshal([]byte(sig.Candidate), &c) == nil {
			_ = p.pc.AddICECandidate(c)
		}
	}
}

// newPeer 构造 peer。created 必须打 s.now()（host 装配平台校准时钟）：
// expire 的未认证窗口以 s.now() 判定，混用原始 time.Now 会在系统钟大
// 偏移的宿主机上让新 peer 立即「高龄」（win 实测 -70s，首个 maintain
// tick 即被踢，hello 来不及完成）。
func (s *Service) newPeer(id string, pc *webrtc.PeerConnection, ctx context.Context, cancel context.CancelFunc) *peer {
	return &peer{s: s, id: id, pc: pc, ctx: ctx, cancel: cancel, created: s.now(), requests: make(chan struct{}, 32)}
}

func (s *Service) offer(sig *protocol.RtcSignal) {
	if len(sig.SDP) == 0 || len(sig.SDP) > protocol.MaxMessageBytes {
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
	p := s.newPeer(sig.PC, pc, ctx, cancel)
	s.pcs[sig.PC] = p
	s.mu.Unlock()
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			b, _ := json.Marshal(c.ToJSON())
			s.cfg.Send(&protocol.RtcSignal{PC: p.id, Kind: protocol.RtcCandidate, Candidate: string(b)})
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
	s.cfg.Send(&protocol.RtcSignal{PC: p.id, Kind: protocol.RtcAnswer, SDP: pc.LocalDescription().SDP})
}
