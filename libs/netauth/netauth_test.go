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
	p := mustNewPolicy(t, NetKeys)
	p.Configure("deny", []string{"allow:10.0.0.1:*", "allow:example.com:443", "deny:bad.com:*"})
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
	p := mustNewPolicy(t, NetKeys)
	p.Configure("open", []string{"deny:bad.com:*"})
	if !p.Allowed("s1", "anything.example", 8080) {
		t.Error("open mode should allow unlisted target")
	}
	if p.Allowed("s1", "bad.com", 443) {
		t.Error("open mode deny rule should still block")
	}
}

func TestAllowedRejectsMalformedTargets(t *testing.T) {
	for _, mode := range []string{"open", "deny"} {
		t.Run(mode, func(t *testing.T) {
			p := mustNewPolicy(t, NetKeys, "localhost:*")
			p.Configure(mode, []string{"allow:example.com:*"})
			for _, target := range []struct {
				host string
				port int
			}{
				{"", 443}, {"*", 443}, {"bad host", 443}, {"https://example.com", 443},
				{"example.com", 0}, {"example.com", -1}, {"example.com", 65536},
				{"localhost", 0},
			} {
				if p.Allowed("s1", target.host, target.port) {
					t.Errorf("malformed target %q:%d was allowed", target.host, target.port)
				}
			}
			if !p.Allowed("s1", "example.com", 443) {
				t.Fatal("valid allowed target was denied")
			}
		})
	}
}

func TestBuiltinLoopback(t *testing.T) {
	p := mustNewPolicy(t, NetKeys, "localhost:*")
	p.Configure("deny", nil)
	if !p.Allowed("s1", "localhost", 3000) {
		t.Error("builtin localhost:* should be allowed in deny mode")
	}
	// cfg deny 行反杀内建（后命中者胜）
	p.Configure("deny", []string{"deny:localhost:*"})
	if p.Allowed("s1", "localhost", 3000) {
		t.Error("net_rules deny localhost:* should override builtin allow")
	}
	// ssh 实例无内建
	q := mustNewPolicy(t, SshKeys)
	q.Configure("deny", nil)
	if q.Allowed("s1", "localhost", 22) {
		t.Error("ssh instance should have no builtin loopback")
	}
}

// TestOrderedFirstMatchWins：有序表核心语义——优先级即书写顺序，无具体度比较。
// 宽 deny 可被后置窄 allow 开洞，窄 deny 也可后置反杀宽 allow；临时 grant
// 不压 deny 终局；Snapshot 按效果分列。
func TestOrderedFirstMatchWins(t *testing.T) {
	p := mustNewPolicy(t, NetKeys)
	// 宽 deny + 后置窄 allow：例外端口放行，其余端口仍拒（deny/open 两模式同效）
	p.Configure("deny", []string{"allow:bad.com:443", "deny:bad.com"})
	if !p.Allowed("s1", "bad.com", 443) {
		t.Error("later allow should poke a hole into wider deny")
	}
	if p.Allowed("s1", "bad.com", 80) {
		t.Error("non-excepted port should stay denied")
	}
	p.Configure("open", []string{"allow:bad.com:443", "deny:bad.com"})
	if !p.Allowed("s1", "bad.com", 443) {
		t.Error("open mode: later allow should win")
	}
	if p.Allowed("s1", "bad.com", 80) {
		t.Error("open mode: non-excepted port should stay denied")
	}
	// 同目标反序：后置 deny 胜
	p.Configure("deny", []string{"deny:bad.com:443", "allow:bad.com:443"})
	if p.Allowed("s1", "bad.com", 443) {
		t.Error("later deny should win over earlier allow")
	}
	// 窄 deny 前置 + 宽 allow 后置 → allow 胜（反杀不再存在，顺序即优先级）
	p.Configure("deny", []string{"allow:bad.com", "deny:bad.com:443"})
	if !p.Allowed("s1", "bad.com", 443) {
		t.Error("later all-port allow should override earlier specific deny")
	}
	if !p.Allowed("s1", "bad.com", 80) {
		t.Error("all-port allow should allow other ports")
	}
	// 临时 grant 插表头压一切（v4 新语义，2026-09-23 拍板「用户点就点了」——
	// 旧「temp 不压 deny 终局」硬底线作废，DenyHit 拒批随 M3c 删除）
	p.Configure("deny", []string{"deny:bad.com"})
	e, _ := ParseEntry("bad.com:443")
	p.Grant("s1", e)
	if !p.Allowed("s1", "bad.com", 443) {
		t.Error("temp grant should top the table (override deny)")
	}
	// 会话隔离：别的 sid 仍被 deny
	if p.Allowed("s2", "bad.com", 443) {
		t.Error("temp grant must be session-scoped")
	}
	p.Configure("deny", []string{"allow:bad.com:443", "deny:other.com:22", "deny:bad.com"})
	snap := p.Snapshot("s1")
	if !snap.Match("bad.com:443") || snap.Match("bad.com:80") || snap.Match("other.com:22") {
		t.Fatal("snapshot must preserve rule priority")
	}

}

func TestTempGrantSessionScope(t *testing.T) {
	p := mustNewPolicy(t, NetKeys)
	p.Configure("deny", nil)
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
	if got := len(allowEntries(p, "s1")); got != 1 {
		t.Errorf("List after idempotent grant = %d entries, want 1", got)
	}
}

func TestSnapshotDenyOverlap(t *testing.T) {
	p := mustNewPolicy(t, NetKeys)
	p.Configure("deny", []string{"deny:bad.com:22"})
	if !deniedTarget(p, Entry{Host: "bad.com", Port: "22"}) {
		t.Error("exact deny entry should hit")
	}
	if !deniedTarget(p, Entry{Host: "bad.com", Port: "*"}) {
		t.Error("wildcard grant overlapping specific deny should hit (conservative)")
	}
	if deniedTarget(p, Entry{Host: "bad.com", Port: "443"}) {
		t.Error("non-overlapping port should not hit")
	}
	if deniedTarget(p, Entry{Host: "good.com", Port: "22"}) {
		t.Error("different host should not hit")
	}
	// 终局语义：前置 allow 行已开洞的目标不再视为 deny（permanent grant 可覆盖）
	p.Configure("deny", []string{"allow:bad.com:22", "deny:bad.com:22"})
	if deniedTarget(p, Entry{Host: "bad.com", Port: "22"}) {
		t.Error("earlier allow must override deny")
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
	p := mustNewPolicy(t, NetKeys)
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

// TestGrantRowOverridesEarlierDeny：permanent grant = 追加到表尾的普通规则行
// （无独立键）——顺序即语义，后命中者胜过上方 deny 行（permission_rules.md §2）。
func TestGrantRowOverridesEarlierDeny(t *testing.T) {
	p := mustNewPolicy(t, NetKeys)
	p.Configure("deny", []string{"allow:meta.example:443", "deny:meta.example:*"})
	if !p.Allowed("s1", "meta.example", 443) {
		t.Error("row appended after deny should override it (last match wins)")
	}
	if p.Allowed("s1", "meta.example", 8080) {
		t.Error("non-granted port should stay denied")
	}
}

// TestPolicyNormalize：非法 policy 值一律归一 deny（安全侧失败）。
func TestPolicyNormalize(t *testing.T) {
	p := mustNewPolicy(t, NetKeys)
	before := p.Mode()
	if err := p.Configure("bogus", nil); err == nil {
		t.Fatal("invalid mode accepted")
	}
	if p.Mode() != before {
		t.Fatal("failed reload changed active mode")
	}
	p.Configure("open", nil)
	if p.Mode() != cfg.PolicyOpen {
		t.Errorf("open policy = %q, want open", p.Mode())
	}
}

func mustNewPolicy(t *testing.T, sel Selector, builtin ...string) *Policy {
	t.Helper()
	p, err := New(sel, builtin...)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func allowEntries(p *Policy, sid string) []string {
	s := p.Snapshot(sid)
	var out []string
	for _, r := range s.Rules {
		if r.Allow && s.Match(r.HostPort) {
			out = append(out, r.HostPort)
		}
	}
	return out
}
func deniedTarget(p *Policy, e Entry) bool {
	allow, row := p.Snapshot("").Resolve(e.String())
	return row >= 0 && !allow
}
