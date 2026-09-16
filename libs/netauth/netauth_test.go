package netauth

import (
	"testing"

	"github.com/veypi/aic-pod/cfg"
)

func TestParseEntry(t *testing.T) {
	cases := []struct {
		in, host, port string
	}{
		{"123.56.83.149:1022", "123.56.83.149", "1022"},
		{"example.com:443", "example.com", "443"},
		{"EXAMPLE.com:443", "example.com", "443"},    // 大小写归一
		{"example.com.:443", "example.com", "443"},   // 尾点归一
		{"example.com", "example.com", "*"},          // bare host → 全端口
		{"root@example.com:22", "example.com", "22"}, // user@ 剥除
		{"localhost:*", "localhost", "*"},
		{"::1", "::1", "*"},                  // bare IPv6
		{"[::1]:22", "::1", "22"},            // 括号 IPv6
		{" 10.0.0.1:080 ", "10.0.0.1", "80"}, // 空白 + 端口零填充归一
	}
	for _, c := range cases {
		e, err := ParseEntry(c.in)
		if err != nil {
			t.Errorf("ParseEntry(%q) error: %v", c.in, err)
			continue
		}
		if e.Host != c.host || e.Port != c.port {
			t.Errorf("ParseEntry(%q) = %s:%s, want %s:%s", c.in, e.Host, e.Port, c.host, c.port)
		}
	}
	bad := []string{"", "  ", "a/b:1", "ex ample.com:22", "x:0", "x:65536", "x:abc", "[::1", "https://x:443", "*", "*:443"}
	for _, s := range bad {
		if _, err := ParseEntry(s); err == nil {
			t.Errorf("ParseEntry(%q) should fail", s)
		}
	}
}

func TestAllowedDenyMode(t *testing.T) {
	p := New(NetKeys)
	p.Configure("deny", []string{"bad.com:*"}, []string{"example.com:443", "10.0.0.1:*"})
	// allow 命中
	if !p.Allowed("s1", "example.com", 443) {
		t.Error("example.com:443 should be allowed")
	}
	// 大小写/尾点归一后命中
	if !p.Allowed("s1", "EXAMPLE.com.", 443) {
		t.Error("EXAMPLE.com.:443 should be allowed (normalized)")
	}
	// 端口不匹配
	if p.Allowed("s1", "example.com", 80) {
		t.Error("example.com:80 should be denied")
	}
	// 全端口条目
	if !p.Allowed("s1", "10.0.0.1", 22) {
		t.Error("10.0.0.1:* should allow any port")
	}
	// 具体度优先：allow 为 * 不压具体端口 deny（同精度/更宽 allow → deny 胜）
	p.Configure("deny", []string{"example.com:443"}, []string{"example.com:*"})
	if p.Allowed("s1", "example.com", 443) {
		t.Error("deny should win over allow")
	}
	// 未列目标
	if p.Allowed("s1", "other.com", 443) {
		t.Error("other.com should be denied in deny mode")
	}
}

func TestAllowedOpenMode(t *testing.T) {
	p := New(NetKeys)
	p.Configure("open", []string{"bad.com:*"}, nil)
	if !p.Allowed("s1", "anything.example", 8080) {
		t.Error("open mode should allow unlisted target")
	}
	if p.Allowed("s1", "bad.com", 443) {
		t.Error("open mode deny list should still block")
	}
}

func TestBuiltinLoopback(t *testing.T) {
	p := New(NetKeys, "localhost:*")
	p.Configure("deny", nil, nil)
	if !p.Allowed("s1", "localhost", 3000) {
		t.Error("builtin localhost:* should be allowed in deny mode")
	}
	// deny 反杀内建
	p.Configure("deny", []string{"localhost:*"}, nil)
	if p.Allowed("s1", "localhost", 3000) {
		t.Error("net_deny localhost:* should override builtin allow")
	}
	// ssh 实例无内建
	q := New(SshKeys)
	q.Configure("deny", nil, nil)
	if q.Allowed("s1", "localhost", 22) {
		t.Error("ssh instance should have no builtin loopback")
	}
}

// TestAllowedSpecificity：具体度优先（端口数字 > *；同精度 deny 胜）——
// 宽 deny 可被窄 allow 压过（端口级例外），窄 deny 仍可反杀宽 allow；
// 临时 grant 不压 deny；Snapshot 剔除被压过的 deny 条目。
func TestAllowedSpecificity(t *testing.T) {
	has := func(list []Entry, host, port string) bool {
		for _, e := range list {
			if e.Host == host && e.Port == port {
				return true
			}
		}
		return false
	}
	p := New(NetKeys)
	// 宽 deny + 窄 allow：例外端口放行，其余端口仍拒（deny/open 两模式同效）
	p.Configure("deny", []string{"bad.com"}, []string{"bad.com:443"})
	if p.Allowed("s1", "bad.com", 443) {
		t.Error("deny must win over specific allow")
	}
	if p.Allowed("s1", "bad.com", 80) {
		t.Error("non-excepted port should stay denied")
	}
	p.Configure("open", []string{"bad.com"}, []string{"bad.com:443"})
	if p.Allowed("s1", "bad.com", 443) {
		t.Error("open mode: deny must win")
	}
	if p.Allowed("s1", "bad.com", 80) {
		t.Error("open mode: non-excepted port should stay denied")
	}
	// 同精度 → deny 胜
	p.Configure("deny", []string{"bad.com:443"}, []string{"bad.com:443"})
	if p.Allowed("s1", "bad.com", 443) {
		t.Error("tie must go to deny")
	}
	// 窄 deny + 宽 allow → deny 胜（反杀，同旧语义）
	p.Configure("deny", []string{"bad.com:443"}, []string{"bad.com"})
	if p.Allowed("s1", "bad.com", 443) {
		t.Error("specific deny should beat all-port allow")
	}
	if !p.Allowed("s1", "bad.com", 80) {
		t.Error("other ports of an all-port allow should stay allowed")
	}
	// 临时 grant 不压 deny
	p.Configure("deny", []string{"bad.com"}, nil)
	e, _ := ParseEntry("bad.com:443")
	p.Grant("s1", e)
	if p.Allowed("s1", "bad.com", 443) {
		t.Error("temp grant must not override deny")
	}
	// Snapshot 剔除被窄 allow 压过的端口 * deny（具体 deny 与 cfg allow 保留）
	p.Configure("deny", []string{"bad.com", "other.com:22"}, []string{"bad.com:443"})
	deny, allow := p.Snapshot("s1")
	if !has(deny, "bad.com", "*") {
		t.Error("deny must stay in snapshot")
	}
	if !has(deny, "other.com", "22") {
		t.Error("specific deny must stay in snapshot")
	}
	if !has(allow, "bad.com", "443") {
		t.Error("cfg allow must stay in snapshot")
	}
}

func TestTempGrantSessionScope(t *testing.T) {
	p := New(NetKeys)
	p.Configure("deny", nil, nil)
	e, _ := ParseEntry("example.com:443")
	p.Grant("s1", e)
	if !p.Allowed("s1", "example.com", 443) {
		t.Error("temp grant should allow sid=s1")
	}
	if p.Allowed("s2", "example.com", 443) {
		t.Error("temp grant leaked across sessions")
	}
	// 幂等
	p.Grant("s1", e)
	if got := len(p.List("s1")); got != 1 {
		t.Errorf("List after idempotent grant = %d entries, want 1", got)
	}
}

func TestDenyHitOverlap(t *testing.T) {
	p := New(NetKeys)
	p.Configure("deny", []string{"bad.com:22"}, nil)
	if !p.DenyHit(Entry{Host: "bad.com", Port: "22"}) {
		t.Error("exact deny entry should hit")
	}
	if !p.DenyHit(Entry{Host: "bad.com", Port: "*"}) {
		t.Error("wildcard grant overlapping specific deny should hit (conservative)")
	}
	if p.DenyHit(Entry{Host: "bad.com", Port: "443"}) {
		t.Error("non-overlapping port should not hit")
	}
	if p.DenyHit(Entry{Host: "good.com", Port: "22"}) {
		t.Error("different host should not hit")
	}
}

// TestReconcileFromCfg：cfg 快照驱动重载（set_config 动态生效向量）。
func TestReconcileFromCfg(t *testing.T) {
	saved := cfg.Global
	defer func() { cfg.Global = saved }()
	o := cfg.NewOptions()
	o.NetPolicy = "deny"
	o.NetAllow = []string{"cfg-example.com:8443"}
	cfg.Global = o
	p := New(NetKeys)
	if !p.Allowed("s1", "cfg-example.com", 8443) {
		t.Error("cfg net_allow entry should be allowed after New/Reconcile")
	}
	o.NetAllow = nil
	cfg.SetAuth(cfg.AuthFrom(o))
	p.Reconcile()
	if p.Allowed("s1", "cfg-example.com", 8443) {
		t.Error("cleared cfg entry should be denied after Reconcile")
	}
}

// TestPolicyNormalize：非法 policy 值一律归一 deny（安全侧失败）。
func TestPolicyNormalize(t *testing.T) {
	p := New(NetKeys)
	p.Configure("bogus", nil, nil)
	if p.Mode() != cfg.PolicyDeny {
		t.Errorf("bogus policy = %q, want deny", p.Mode())
	}
	p.Configure("open", nil, nil)
	if p.Mode() != cfg.PolicyOpen {
		t.Errorf("open policy = %q, want open", p.Mode())
	}
}
