package host

import (
	"testing"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/netauth"
	"github.com/veypi/aic-pod/libs/proto"
)

// grantTarget 域路由与 deny 拒绝（temp 范围，不触盘——permanent 落盘路径见
// TestPersistGrantAppendsRuleRow）。argv 解析在 glue 平台命令层
//（aic-pod/libs/vsh/cmds.go），本包只承接已解析的域+目标。
func TestRunGrantTarget(t *testing.T) {
	saved := cfg.Global
	defer func() { cfg.Global = saved }()
	cfg.Global = cfg.NewOptions()
	c := &Client{
		netPol: netauth.New(netauth.NetKeys),
		sshPol: netauth.New(netauth.SshKeys),
	}
	c.netPol.Configure("deny", []string{"deny:bad.com:22"})

	// deny 重叠 → rejected
	r := c.grantTarget("s1", "m1", "net", "bad.com:22", false)
	if r.State != proto.StateRejected {
		t.Fatalf("grant net bad.com:22 state = %s, want rejected (%s)", r.State, r.Error)
	}
	// temp 授权生效（目标入 sid 名单）
	r = c.grantTarget("s1", "m2", "net", "example.com:443", false)
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
	r = c.grantTarget("s1", "m3", "ssh", "10.0.0.2:22", false)
	if r.State != proto.StateCompleted {
		t.Fatalf("grant ssh state = %s (%s)", r.State, r.Error)
	}
	if !c.sshPol.Allowed("s1", "10.0.0.2", 22) {
		t.Fatal("ssh grant not in sshPol")
	}
	if c.netPol.Allowed("s1", "10.0.0.2", 22) {
		t.Fatal("ssh grant leaked into netPol")
	}
}

func TestExecGrantIsLocal(t *testing.T) {
	saved := cfg.Global
	cfg.Global = cfg.NewOptions()
	defer func() { cfg.Global = saved }()
	cfg.Global.ExecPolicy = cfg.PolicyDeny
	cfg.Global.ExecDeny = []string{"bash"}
	c, _ := testClient(t)
	if c.execAllowed("s1", "json") {
		t.Fatal("ungranted command allowed")
	}
	// exec wire 命令恒可用（script 契约的唯一入口；内容由规则表与 native 白名单门控）。
	if !c.execAllowed("s1", "exec") {
		t.Fatal("local exec unavailable")
	}
}

// TestPersistGrantAppendsRuleRow：--permanent 把规则行追加到 <域>_rules 表尾
// （无独立 grants 键），幂等归一不产生重复行。
func TestPersistGrantAppendsRuleRow(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv("APPDATA", dir)
	saved := cfg.Global
	t.Cleanup(func() { cfg.Global = saved })
	c, _ := testClient(t)
	cfg.Global = cfg.NewOptions()
	if err := c.persistGrant("net", "example.com:443"); err != nil {
		t.Fatal(err)
	}
	if err := c.persistGrant("net", "example.com:443"); err != nil { // 幂等
		t.Fatal(err)
	}
	o, err := cfg.LoadFile()
	if err != nil {
		t.Fatal(err)
	}
	if len(o.NetRules) != 1 || o.NetRules[0] != "allow:example.com:443" {
		t.Fatalf("net_rules = %v, want exactly [allow:example.com:443]", o.NetRules)
	}
}
