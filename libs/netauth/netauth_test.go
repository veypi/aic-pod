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
	p.Configure("deny", []string{"deny:bad.com:*", "allow:example.com:443", "allow:10.0.0.1:*"}, nil)
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
	// 未列目标
	if p.Allowed("s1", "other.com", 443) {
		t.Error("other.com should be denied in deny mode")
	}
}

func TestAllowedOpenMode(t *testing.T) {
	p := New(NetKeys)
	p.Configure("open", []string{"deny:bad.com:*"}, nil)
	if !p.Allowed("s1", "anything.example", 8080) {
		t.Error("open mode should allow unlisted target")
	}
	if p.Allowed("s1", "bad.com", 443) {
		t.Error("open mode deny rule should still block")
	}
}

func TestBuiltinLoopback(t *testing.T) {
	p := New(NetKeys, "localhost:*")
	p.Configure("deny", nil, nil)
	if !p.Allowed("s1", "localhost", 3000) {
		t.Error("builtin localhost:* should be allowed in deny mode")
	}
	// cfg deny 行反杀内建（后命中者胜）
	p.Configure("deny", []string{"deny:localhost:*"}, nil)
	if p.Allowed("s1", "localhost", 3000) {
		t.Error("net_rules deny localhost:* should override builtin allow")
	}
	// ssh 实例无内建
	q := New(SshKeys)
	q.Configure("deny", nil, nil)
	if q.Allowed("s1", "localhost", 22) {
		t.Error("ssh instance should have no builtin loopback")
	}
}

// TestOrderedLastMatchWins：有序表核心语义——优先级即书写顺序，无具体度比较。
// 宽 deny 可被后置窄 allow 开洞，窄 deny 也可后置反杀宽 allow；临时 grant
// 不压 deny 终局；Snapshot 按效果分列。
func TestOrderedLastMatchWins(t *testing.T) {
	has := func(list []Entry, host, port string) bool {
		for _, e := range list {
			if e.Host == host && e.Port == port {
				return true
			}
		}
		return false
	}
	p := New(NetKeys)
	// 宽 deny + 后置窄 allow：例外端口放行，其余端口仍拒（deny/open 两模式同效）
	p.Configure("deny", []string{"deny:bad.com", "allow:bad.com:443"}, nil)
	if !p.Allowed("s1", "bad.com", 443) {
		t.Error("later allow should poke a hole into wider deny")
	}
	if p.Allowed("s1", "bad.com", 80) {
		t.Error("non-excepted port should stay denied")
	}
	p.Configure("open", []string{"deny:bad.com", "allow:bad.com:443"}, nil)
	if !p.Allowed("s1", "bad.com", 443) {
		t.Error("open mode: later allow should win")
	}
	if p.Allowed("s1", "bad.com", 80) {
		t.Error("open mode: non-excepted port should stay denied")
	}
	// 同目标反序：后置 deny 胜
	p.Configure("deny", []string{"allow:bad.com:443", "deny:bad.com:443"}, nil)
	if p.Allowed("s1", "bad.com", 443) {
		t.Error("later deny should win over earlier allow")
	}
	// 窄 deny 前置 + 宽 allow 后置 → allow 胜（反杀不再存在，顺序即优先级）
	p.Configure("deny", []string{"deny:bad.com:443", "allow:bad.com"}, nil)
	if !p.Allowed("s1", "bad.com", 443) {
		t.Error("later all-port allow should override earlier specific deny")
	}
	if !p.Allowed("s1", "bad.com", 80) {
		t.Error("all-port allow should allow other ports")
	}
	// 临时 grant 不压 deny 终局
	p.Configure("deny", []string{"deny:bad.com"}, nil)
	e, _ := ParseEntry("bad.com:443")
	p.Grant("s1", e)
	if p.Allowed("s1", "bad.com", 443) {
		t.Error("temp grant must not override a deny outcome")
	}
	// Snapshot 按效果分列（deny 行与 allow 行各归其列）
	p.Configure("deny", []string{"deny:bad.com", "deny:other.com:22", "allow:bad.com:443"}, nil)
	deny, allow := p.Snapshot("s1")
	if !has(deny, "bad.com", "*") || !has(deny, "other.com", "22") {
		t.Error("deny rows missing from snapshot")
	}
	if !has(allow, "bad.com", "443") {
		t.Error("allow rows missing from snapshot")
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
	p.Configure("deny", []string{"deny:bad.com:22"}, nil)
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
	// 终局语义：后置 allow 行已开洞的目标不再视为 deny（permanent grant 可覆盖）
	p.Configure("deny", []string{"deny:bad.com:22", "allow:bad.com:22"}, nil)
	if p.DenyHit(Entry{Host: "bad.com", Port: "22"}) {
		t.Error("overridden deny outcome should not report DenyHit")
	}
	// LastDenyRow：最后重叠的 deny 行（1 起）
	if row, raw, ok := p.LastDenyRow(Entry{Host: "bad.com", Port: "22"}); !ok || row != 1 || raw != "bad.com:22" {
		t.Errorf("LastDenyRow = %d %q %v, want 1 bad.com:22 true", row, raw, ok)
	}
}

// TestReconcileFromCfg：cfg 快照驱动重载（set_config 动态生效向量）。
func TestReconcileFromCfg(t *testing.T) {
	saved := cfg.Global
	defer func() { cfg.Global = saved }()
	o := cfg.NewOptions()
	o.NetPolicy = "deny"
	o.NetRules = []string{"allow:cfg-example.com:8443"}
	cfg.Global = o
	p := New(NetKeys)
	if !p.Allowed("s1", "cfg-example.com", 8443) {
		t.Error("cfg net_rules entry should be allowed after New/Reconcile")
	}
	o.NetRules = nil
	cfg.SetAuth(cfg.AuthFrom(o))
	p.Reconcile()
	if p.Allowed("s1", "cfg-example.com", 8443) {
		t.Error("cleared cfg entry should be denied after Reconcile")
	}
}

// TestPermanentGrantsAppendedAfterRules：grants 键行恒在 cfg rules 之后——
// permanent grant 覆盖 deny 行合法（permission_rules.md §2）。
func TestPermanentGrantsAppendedAfterRules(t *testing.T) {
	p := New(NetKeys)
	p.Configure("deny", []string{"deny:meta.example:*"}, []string{"allow:meta.example:443"})
	if !p.Allowed("s1", "meta.example", 443) {
		t.Error("permanent grant row should override cfg deny row")
	}
	if p.Allowed("s1", "meta.example", 8080) {
		t.Error("non-granted port should stay denied")
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
