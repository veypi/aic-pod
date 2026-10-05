package rtc

// 认证租约直接存 peer（四仓精简 §6，2026-10-06）：认证成功后的身份、
// connectionID、绑定指纹和 leaseUntil 都在 peer 上；Service 只保留一次性
// ticket 消费缓存（验签材料来自 Config）。hello/renew 在已知 peer 上核对
// 票据与真实 DTLS 指纹（指纹来自传输层，绝不取自请求参数）。请求验证读
// peer 当前租约（含续租后期限），不是第一次认证的快照。不再有第二张
// 连接表（hostauth.Access 删除）。

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/veypi/aic-pod/protocol"
)

// peerLease 是 peer 的认证租约（p.mu 守卫；until 续租可延长）。
type peerLease struct {
	subject, connectionID, sessionID, fingerprint string
	until                                         time.Time
}

func (s *Service) now() time.Time {
	if s.cfg.Now != nil {
		return s.cfg.Now()
	}
	return time.Now()
}

// verifyTicket 验签并核对绑定：host/user/凭据版本/pcID/真实 DTLS 指纹。
func (s *Service) verifyTicket(ticket, pcID, fingerprint string) (protocol.RtcTicket, error) {
	t, err := protocol.VerifyRtcTicket(s.cfg.Key, ticket, s.now())
	if err != nil {
		return t, err
	}
	fp, err := protocol.NormalizeFingerprint(fingerprint)
	if err != nil || t.HostID != s.cfg.HostID || t.UserID != s.cfg.UserID || t.CredentialVersion != s.cfg.CredentialVersion || t.PCID != pcID || t.Fingerprint != fp {
		return protocol.RtcTicket{}, protocol.Fail("unauthorized", "Direct ticket does not match this DTLS connection")
	}
	return t, nil
}

// consumeTicket 一次性消费（重放拒绝；过期条目由 maintain 周期清扫）。
func (s *Service) consumeTicket(t protocol.RtcTicket) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.consumed[t.ID]; exists {
		return protocol.Fail("unauthorized", "Direct ticket has already been used")
	}
	if len(s.consumed) >= 32768 {
		return protocol.Fail("overloaded", "Ticket admission quota reached")
	}
	s.consumed[t.ID] = time.Unix(t.ExpiresAt, 0)
	return nil
}

// admit hello/open：建立 peer 租约（票据不得是续租票据；同一 peer 只认证
// 一次——pcs 按 pcID 唯一，跨 peer 的重复 pcID 认证按构造不存在）。
func (p *peer) admit(ticket, fingerprint string) (map[string]any, error) {
	t, err := p.s.verifyTicket(ticket, p.id, fingerprint)
	if err != nil {
		return nil, err
	}
	if t.ConnectionID != "" {
		return nil, protocol.Fail("unauthorized", "A renewal ticket cannot open a connection")
	}
	p.mu.Lock()
	if p.closed || p.lease != nil {
		p.mu.Unlock()
		return nil, protocol.Fail("unauthorized", "Peer already authenticated or closed")
	}
	p.mu.Unlock()
	if err := p.s.consumeTicket(t); err != nil {
		return nil, err
	}
	lease := &peerLease{
		subject: t.UserID, connectionID: protocol.NewID("conn_"),
		sessionID: t.SessionID, fingerprint: t.Fingerprint,
		until: time.Unix(t.LeaseUntil, 0),
	}
	p.mu.Lock()
	if p.closed || p.lease != nil {
		p.mu.Unlock()
		return nil, protocol.Fail("unauthorized", "Peer already authenticated or closed")
	}
	p.lease = lease
	p.mu.Unlock()
	return map[string]any{"host_id": p.s.cfg.HostID, "connection_id": lease.connectionID, "expires_at": lease.until.UnixMilli()}, nil
}

// renew 续租：票据必须绑定本 peer 的活跃租约（connectionID/指纹一致），
// 只延长期限不回拨。
func (p *peer) renew(ticket, fingerprint string) (map[string]bool, error) {
	t, err := p.s.verifyTicket(ticket, p.id, fingerprint)
	if err != nil {
		return nil, err
	}
	lease := p.currentLease()
	if lease == nil || t.ConnectionID != lease.connectionID || t.Fingerprint != lease.fingerprint {
		return nil, protocol.Fail("unauthorized", "Renewal does not match the active connection")
	}
	if err := p.s.consumeTicket(t); err != nil {
		return nil, err
	}
	until := time.Unix(t.LeaseUntil, 0)
	p.mu.Lock()
	if until.After(p.lease.until) {
		p.lease.until = until
		lease.until = until
	}
	p.mu.Unlock()
	return map[string]bool{"renewed": true}, nil
}

// currentLease 读 peer 当前租约（含续租后期限）；未认证或已过期 = nil。
func (p *peer) currentLease() *peerLease {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lease == nil || !p.lease.until.After(p.s.now()) {
		return nil
	}
	lease := *p.lease
	return &lease
}

// fingerprint 取传输层真实 DTLS 指纹（票据核对基准；不经请求参数）。
func (p *peer) fingerprint() (string, error) {
	if p.pc.SCTP() == nil || p.pc.SCTP().Transport() == nil {
		return "", protocol.Fail("unauthorized", "DTLS is unavailable")
	}
	cert := p.pc.SCTP().Transport().GetRemoteCertificate()
	if len(cert) == 0 {
		return "", protocol.Fail("unauthorized", "Peer certificate unavailable")
	}
	sum := sha256.Sum256(cert)
	return protocol.NormalizeFingerprint("sha-256 " + hex.EncodeToString(sum[:]))
}
