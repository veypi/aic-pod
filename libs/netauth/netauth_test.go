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
	// deny 恒优先（即使 allow 也含）
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
