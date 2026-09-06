package host

import (
	"testing"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/netauth"
	"github.com/veypi/aic-pod/libs/proto"
)

func TestParseGrantArgv(t *testing.T) {
	cases := []struct {
		argv      []string
		domain    string
		target    string
		permanent bool
	}{
		{[]string{"fs", "/tmp/x"}, "fs", "/tmp/x", false},
		{[]string{"net", "example.com:443", "--permanent"}, "net", "example.com:443", true},
		{[]string{"ssh", "example.com"}, "ssh", "example.com", false},
		{[]string{"--permanent", "FS", "/a"}, "fs", "/a", true}, // flag 位置任意；域名小写归一
		{[]string{"net", "1.2.3.4:22", "--temp"}, "net", "1.2.3.4:22", false},
	}
	for _, c := range cases {
		d, tg, pm, err := parseGrantArgv(c.argv)
		if err != nil {
			t.Errorf("parseGrantArgv(%v) error: %v", c.argv, err)
			continue
		}
		if d != c.domain || tg != c.target || pm != c.permanent {
			t.Errorf("parseGrantArgv(%v) = (%q,%q,%v), want (%q,%q,%v)",
				c.argv, d, tg, pm, c.domain, c.target, c.permanent)
		}
	}
	bad := [][]string{
		{},                        // 空
		{"fs"},                    // 缺目标
		{"/tmp/x"},                // 缺域（旧 grant_apply 形态必须报错而非误判）
		{"fs", "/a", "/b"},        // 多目标
		{"fs", "/a", "--unknown"}, // 未知 flag
	}
	for _, argv := range bad {
		if _, _, _, err := parseGrantArgv(argv); err == nil {
			t.Errorf("parseGrantArgv(%v) should fail", argv)
		}
	}
}

// runGrant 域路由与 deny 拒绝（temp 范围，不触盘——permanent 走 persistGrant
// 落盘路径，不在单测覆盖）。
func TestRunGrantTarget(t *testing.T) {
	saved := cfg.Global
	defer func() { cfg.Global = saved }()
	cfg.Global = cfg.NewOptions()
	c := &Client{
		netPol: netauth.New(netauth.NetKeys),
		sshPol: netauth.New(netauth.SshKeys),
	}
	c.netPol.Configure("deny", []string{"bad.com:22"}, nil)

	// deny 重叠 → rejected
	r := c.runGrant("s1", "m1", []string{"net", "bad.com:22"})
	if r.State != proto.StateRejected {
		t.Fatalf("grant net bad.com:22 state = %s, want rejected (%s)", r.State, r.Error)
	}
	// temp 授权生效（目标入 sid 名单）
	r = c.runGrant("s1", "m2", []string{"net", "example.com:443"})
	if r.State != proto.StateCompleted {
		t.Fatalf("grant net example.com:443 state = %s (%s)", r.State, r.Error)
	}
	if !c.netPol.Allowed("s1", "example.com", 443) {
		t.Fatal("temp grant not effective for s1")
	}
	if c.netPol.Allowed("s2", "example.com", 443) {
		t.Fatal("temp grant leaked across sessions")
	}
	// 域路由：ssh 目标入 sshPol 而非 netPol
	r = c.runGrant("s1", "m3", []string{"ssh", "10.0.0.2:22"})
	if r.State != proto.StateCompleted {
		t.Fatalf("grant ssh state = %s (%s)", r.State, r.Error)
	}
	if !c.sshPol.Allowed("s1", "10.0.0.2", 22) {
		t.Fatal("ssh grant not in sshPol")
	}
	if c.netPol.Allowed("s1", "10.0.0.2", 22) {
		t.Fatal("ssh grant leaked into netPol")
	}
	// 未知域报错
	if r = c.runGrant("s1", "m4", []string{"xyz", "a:1"}); r.State != proto.StateError {
		t.Fatalf("unknown domain state = %s, want error", r.State)
	}
	// 旧 grant_apply 裸路径形态 → error（域缺失）而非误判
	if r = c.runGrant("s1", "m5", []string{"/tmp/x"}); r.State != proto.StateError {
		t.Fatalf("legacy bare-path form state = %s, want error", r.State)
	}
}
