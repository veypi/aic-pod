// Copyright (C) 2025 veypi <i@veypi.com>
// Distributed under terms of the MIT license.

// Package rtc 是 host 的 WebRTC 直连应答服务（2026-09-10 设计，README 同源）：
//
// 页面（任意浏览器/手机，含 https 平台页）经 NATS 信令与 host 建立
// RTCPeerConnection，数据面为 DataChannel（DTLS 强制加密 + UDP host
// candidate LAN 直连）——不受浏览器混合内容/PNA 限制，无需 CA 证书
//（身份锚点 = 经认证信令通道交换的 SDP 指纹）。
//
// 链路：页面 offer → u.{uid}.h.host_{id}.rtc.in（host 通配 inbox 覆盖）→
// 本服务应答 answer/candidate → u.{uid}.h.{id}.{ver}.rtc → ICE 连通检查 →
// DTLS → DataChannel("fs") 打开 → 鉴权帧 {code}（与本地管理 API 的
// x-aic-code 同源，5 次失败锁 1 分钟）→ fs 帧协议服务。
//
// 信任级：code 鉴权通过 = 本地控制台信任级（fs 调用 granted=9，审批不再
// 出现；fsauth 三域 deny/allow 策略照常生效——deny 恒拒不可绕过）。
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
	"github.com/veypi/aic-pod/libs/proto"
	"github.com/veypi/aic-pod/libs/vcore"
)

// Config 是 Service 的装配参数（由 host client 接线）。
type Config struct {
	// Code 本地校验码（cfg.Options.Code，进程级）；空 = 服务不启动。
	Code string
	// HostID/Hostname/Version 用于鉴权成功应答（页面核对 host_id 防错连）。
	HostID   string
	Hostname string
	Version  string
	// Send 信令出向发布（host client 接到 NATS 的 RtcOutSubject）。
	Send func(sig *proto.RtcSignal)
	// RunFS 是 fs 帧协议的执行体（host client 注入：vcore.RunFS + OSVFS +
	// fsauth 视图，granted=9 本地控制台信任级）。
	RunFS func(ctx context.Context, raw json.RawMessage) (*vcore.Result, error)
	// ReadBin 是 readbin op 的执行体（host client 注入：vcore.ReadBin + fsauth
	// 视图，同信任级）。返回 (字节, mime, 文件总字节数)；区间与上限约束由
	// vcore.ReadBin 保证（含 deny 恒拒）。
	ReadBin func(path string, off, length int64) ([]byte, string, int64, error)
	Logf  func(format string, args ...any)
}

// Service 是 RTC 应答服务：单 UDP mux（随机端口，全部 PeerConnection 复用，
// 防火墙友好）+ mDNS 解析（浏览器侧候选为 .local 化名，QueryOnly 模式应答方
// 可解析）。零 ICE servers——LAN host candidate 直连，不依赖 STUN/TURN。
//
// maxPeerConnections 是并发连接上限（2026-09-10 评审）：信令不需验签，owner
// 域内任意端可发 offer，无上限会被洪泛耗尽 DTLS/UDP 资源；多 tab/多页面
// 正常并发远小于此值。
const maxPeerConnections = 16

type Service struct {
	cfg  Config
	api  *webrtc.API
	conn *net.UDPConn

	mu      sync.Mutex
	pcs     map[string]*webrtc.PeerConnection // pc_id → 连接
	failCnt int
	lockEnd time.Time

	logf func(string, ...any)
}

// New 创建并启动 RTC 应答服务（UDP mux 监听随机端口，不暴露任何 TCP/HTTP）。
func New(cfg Config) (*Service, error) {
	if cfg.Code == "" {
		return nil, fmt.Errorf("rtc: code is empty")
	}
	if cfg.Send == nil || cfg.RunFS == nil || cfg.ReadBin == nil {
		return nil, fmt.Errorf("rtc: Send, RunFS and ReadBin are required")
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: 0})
	if err != nil {
		return nil, fmt.Errorf("rtc: udp listen: %w", err)
	}
	se := webrtc.SettingEngine{}
	se.SetICEUDPMux(ice.NewUDPMuxDefault(ice.UDPMuxParams{UDPConn: conn}))
	// 浏览器默认把本机候选 mDNS 化名（*.local）；应答方开 QueryOnly 解析之。
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeQueryOnly)
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	s := &Service{
		cfg:  cfg,
		api:  webrtc.NewAPI(webrtc.WithSettingEngine(se)),
		conn: conn,
		pcs:  map[string]*webrtc.PeerConnection{},
		logf: logf,
	}
	s.logf("rtc: listening udp :%d (mux)", conn.LocalAddr().(*net.UDPAddr).Port)
	return s, nil
}

// Close 关闭全部连接与 UDP socket（幂等）。
func (s *Service) Close() {
	s.mu.Lock()
	pcs := s.pcs
	s.pcs = map[string]*webrtc.PeerConnection{}
	s.mu.Unlock()
	for _, pc := range pcs {
		_ = pc.Close()
	}
	_ = s.conn.Close()
}

// HandleSignal 处理一条 rtc.in 信令（host client 从 NATS 通配订阅路由过来）。
func (s *Service) HandleSignal(sig *proto.RtcSignal) {
	if sig == nil || sig.PC == "" {
		return
	}
	switch sig.Kind {
	case proto.RtcOffer:
		s.handleOffer(sig)
	case proto.RtcCandidate:
		if pc := s.getPC(sig.PC); pc != nil && sig.Candidate != "" {
			var init webrtc.ICECandidateInit
			if err := json.Unmarshal([]byte(sig.Candidate), &init); err == nil {
				if err := pc.AddICECandidate(init); err != nil {
					s.logf("rtc: add candidate (pc=%s): %v", sig.PC, err)
				}
			}
		}
	case proto.RtcBye:
		s.dropPC(sig.PC)
	}
}

func (s *Service) getPC(id string) *webrtc.PeerConnection {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pcs[id]
}

func (s *Service) dropPC(id string) {
	s.mu.Lock()
	pc := s.pcs[id]
	delete(s.pcs, id)
	s.mu.Unlock()
	if pc != nil {
		_ = pc.Close()
	}
}

// handleOffer 应答一个 SDP offer：建 PeerConnection → 收 DataChannel →
// 应答 answer，trickle 候选经 OnICECandidate 逐个回发。
func (s *Service) handleOffer(sig *proto.RtcSignal) {
	if sig.SDP == "" {
		return
	}
	// 同 pc_id 重复 offer = 页面重试：丢弃旧连接重建。
	s.dropPC(sig.PC)
	// 连接数上限（2026-09-10 评审）：防信令面洪泛耗尽 DTLS/UDP 资源——
	// 信令不需验签，owner 域内任意端可发 offer，必须有关口。
	s.mu.Lock()
	full := len(s.pcs) >= maxPeerConnections
	s.mu.Unlock()
	if full {
		s.logf("rtc: pc cap (%d) reached, refusing offer pc=%s", maxPeerConnections, sig.PC)
		return
	}

	pc, err := s.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		s.logf("rtc: new peer connection: %v", err)
		return
	}
	s.mu.Lock()
	s.pcs[sig.PC] = pc
	s.mu.Unlock()

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		data, err := json.Marshal(c.ToJSON())
		if err != nil {
			return
		}
		s.cfg.Send(&proto.RtcSignal{PC: sig.PC, Kind: proto.RtcCandidate, Candidate: string(data)})
	})
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		s.logf("rtc: pc=%s state=%s", sig.PC, st)
		switch st {
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed, webrtc.PeerConnectionStateDisconnected:
			s.dropPC(sig.PC)
		}
	})
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		if dc.Label() != fsChannelLabel {
			return
		}
		s.serveFSChannel(sig.PC, dc)
	})
	// 建连看门狗：60s 未 connected 即回收（页面中途放弃/网络不通）。
	time.AfterFunc(60*time.Second, func() {
		if pc.ConnectionState() == webrtc.PeerConnectionStateNew ||
			pc.ConnectionState() == webrtc.PeerConnectionStateConnecting {
			s.logf("rtc: pc=%s connect watchdog expired", sig.PC)
			s.dropPC(sig.PC)
		}
	})

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer, SDP: sig.SDP,
	}); err != nil {
		s.logf("rtc: pc=%s set remote: %v", sig.PC, err)
		s.dropPC(sig.PC)
		return
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		s.logf("rtc: pc=%s create answer: %v", sig.PC, err)
		s.dropPC(sig.PC)
		return
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		s.logf("rtc: pc=%s set local: %v", sig.PC, err)
		s.dropPC(sig.PC)
		return
	}
	s.cfg.Send(&proto.RtcSignal{PC: sig.PC, Kind: proto.RtcAnswer, SDP: pc.LocalDescription().SDP})
}
