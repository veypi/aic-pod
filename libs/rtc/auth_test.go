package rtc

// peer 租约白盒测试（§6 验收面）：ticket 重放/续租/过期/指纹与 pcID 绑定。
// 旧 hostauth.Access 的对应覆盖随删包迁移至此。

import (
	"context"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/veypi/aic-pod/protocol"
)

func authTestService(t *testing.T) (*Service, []byte) {
	t.Helper()
	key, err := protocol.RtcDirectKey("secret", "host_1")
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{
		cfg: Config{HostID: "host_1", UserID: "owner", CredentialVersion: 1, Key: key,
			Dispatch:   func(context.Context, protocol.Caller, protocol.Request) protocol.Response { return protocol.Response{} },
			Disconnect: func(protocol.Caller) {},
			Send:       func(*protocol.RtcSignal) {}},
		pcs:      map[string]*peer{},
		consumed: map[string]time.Time{},
	}
	return svc, key
}

func authTestPeer(svc *Service, id string) *peer {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		panic(err)
	}
	return &peer{s: svc, id: id, pc: pc, cancel: func() {}}
}

const testFingerprint = "sha-256 00:01:02:03:04:05:06:07:08:09:0A:0B:0C:0D:0E:0F:10:11:12:13:14:15:16:17:18:19:1A:1B:1C:1D:1E:1F"

func sign(t *testing.T, key []byte, ticket protocol.RtcTicket) string {
	t.Helper()
	raw, err := protocol.SignRtcTicket(key, ticket, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestPeerAdmitAndReplay(t *testing.T) {
	svc, key := authTestService(t)
	p1 := authTestPeer(svc, "pc_1")
	ticket := sign(t, key, protocol.RtcTicket{HostID: "host_1", UserID: "owner", CredentialVersion: 1, PCID: "pc_1", Fingerprint: testFingerprint, SessionID: "s1"})
	value, err := p1.admit(ticket, testFingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if value["connection_id"] == "" || value["expires_at"] == nil {
		t.Fatalf("admit value = %+v", value)
	}
	// 同一 peer 重复认证拒绝。
	if _, err := p1.admit(ticket, testFingerprint); err == nil {
		t.Fatal("duplicate admit on same peer admitted")
	}
	// 同一票据在另一 peer 重放拒绝（一次性消费）。
	p2 := authTestPeer(svc, "pc_1")
	if _, err := p2.admit(ticket, testFingerprint); err == nil {
		t.Fatal("ticket replay admitted")
	}
	// 租约可读且绑定票据字段。
	lease := p1.currentLease()
	if lease == nil || lease.subject != "owner" || lease.sessionID != "s1" || lease.connectionID != value["connection_id"] {
		t.Fatalf("lease = %+v", lease)
	}
}

func TestPeerAdmitBindingChecks(t *testing.T) {
	svc, key := authTestService(t)
	// pcID 绑定：票据指向别的 pc 拒绝。
	wrongPC := sign(t, key, protocol.RtcTicket{HostID: "host_1", UserID: "owner", CredentialVersion: 1, PCID: "pc_other", Fingerprint: testFingerprint})
	if _, err := authTestPeer(svc, "pc_1").admit(wrongPC, testFingerprint); err == nil {
		t.Fatal("ticket for another pc admitted")
	}
	// 指纹不符拒绝。
	ticket := sign(t, key, protocol.RtcTicket{HostID: "host_1", UserID: "owner", CredentialVersion: 1, PCID: "pc_1", Fingerprint: testFingerprint})
	otherFP := "sha-256 FF:01:02:03:04:05:06:07:08:09:0A:0B:0C:0D:0E:0F:10:11:12:13:14:15:16:17:18:19:1A:1B:1C:1D:1E:1F"
	if _, err := authTestPeer(svc, "pc_1").admit(ticket, otherFP); err == nil {
		t.Fatal("fingerprint mismatch admitted")
	}
	// 续租票据不能开连接。
	renewal := sign(t, key, protocol.RtcTicket{HostID: "host_1", UserID: "owner", CredentialVersion: 1, PCID: "pc_1", Fingerprint: testFingerprint, ConnectionID: "conn_x"})
	if _, err := authTestPeer(svc, "pc_1").admit(renewal, testFingerprint); err == nil {
		t.Fatal("renewal ticket opened a connection")
	}
	// 未认证续租拒绝。
	if _, err := authTestPeer(svc, "pc_1").renew(renewal, testFingerprint); err == nil {
		t.Fatal("renew without active lease admitted")
	}
}

func TestPeerRenewExtendsLease(t *testing.T) {
	svc, key := authTestService(t)
	p := authTestPeer(svc, "pc_1")
	ticket := sign(t, key, protocol.RtcTicket{HostID: "host_1", UserID: "owner", CredentialVersion: 1, PCID: "pc_1", Fingerprint: testFingerprint})
	value, err := p.admit(ticket, testFingerprint)
	if err != nil {
		t.Fatal(err)
	}
	connID := value["connection_id"].(string)
	// 伪造临近到期的租约，续租必须延长期限。
	p.mu.Lock()
	p.lease.until = time.Now().Add(time.Minute)
	p.mu.Unlock()
	before := p.currentLease().until
	renewal := sign(t, key, protocol.RtcTicket{HostID: "host_1", UserID: "owner", CredentialVersion: 1, PCID: "pc_1", Fingerprint: testFingerprint, ConnectionID: connID})
	if _, err := p.renew(renewal, testFingerprint); err != nil {
		t.Fatal(err)
	}
	if after := p.currentLease().until; !after.After(before) {
		t.Fatalf("renew did not extend lease: %v → %v", before, after)
	}
	// 续租票据重放拒绝。
	if _, err := p.renew(renewal, testFingerprint); err == nil {
		t.Fatal("renewal replay admitted")
	}
	// 绑定别的 connectionID 的续租拒绝。
	alien := sign(t, key, protocol.RtcTicket{HostID: "host_1", UserID: "owner", CredentialVersion: 1, PCID: "pc_1", Fingerprint: testFingerprint, ConnectionID: "conn_other"})
	if _, err := p.renew(alien, testFingerprint); err == nil {
		t.Fatal("renewal for another connection admitted")
	}
	// toolCaller 读当前租约（续租后期限）。
	caller, err := p.toolCaller()
	if err != nil {
		t.Fatal(err)
	}
	if !caller.Expiry().After(before) || caller.Subject != "owner" || caller.ConnectionID != connID {
		t.Fatalf("caller does not reflect renewed lease: %+v", caller)
	}
}

func TestPeerLeaseExpiry(t *testing.T) {
	svc, key := authTestService(t)
	p := authTestPeer(svc, "pc_1")
	ticket := sign(t, key, protocol.RtcTicket{HostID: "host_1", UserID: "owner", CredentialVersion: 1, PCID: "pc_1", Fingerprint: testFingerprint})
	if _, err := p.admit(ticket, testFingerprint); err != nil {
		t.Fatal(err)
	}
	// 租约过期：currentLease/toolCaller 拒绝，expire 丢 peer。
	p.mu.Lock()
	p.lease.until = time.Now().Add(-time.Second)
	p.mu.Unlock()
	if p.currentLease() != nil {
		t.Fatal("expired lease still active")
	}
	if _, err := p.toolCaller(); err == nil {
		t.Fatal("expired lease issued a caller")
	}
	svc.pcs[p.id] = p
	p.expire(time.Now())
	if !p.closed {
		t.Fatal("expired peer not dropped")
	}
	// 未认证 peer 超窗丢弃。
	fresh := authTestPeer(svc, "pc_2")
	fresh.created = time.Now().Add(-time.Minute)
	svc.pcs[fresh.id] = fresh
	fresh.expire(time.Now())
	if !fresh.closed {
		t.Fatal("stale unauthenticated peer not dropped")
	}
}
