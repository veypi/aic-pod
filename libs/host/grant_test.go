package host

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/vbox"
)

// grantTarget 域路由与 temp 授权（不触盘——permanent 落盘路径见
// TestPersistGrantAppendsRuleRow）。argv 解析在 glue 平台命令层
// （aic-pod/libs/execution/cmds.go），本包只承接已解析的域+目标。
// M3c：DenyHit 拒批已废（temp 行插表头可覆盖 deny——「用户点就点了」）。
func TestRunGrantTarget(t *testing.T) {
	withGlobal(t, func(o *cfg.Options) {
		o.NetPolicy = cfg.PolicyDeny
		o.NetRules = []string{"deny:bad.com:22"}
		o.SshPolicy = cfg.PolicyDeny
	})
	c := &Client{perms: newTestPerms(t, "")}

	// deny 目标同样可授（temp 行插表头压一切——2.7.4 拒批删除后的新语义）
	if _, err := c.grantTarget("s1", "net", "bad.com:22", false); err != nil {
		t.Fatal(err)
	}
	if !c.perms.netSnapshot("s1").Match("bad.com:22") {
		t.Fatal("temp grant over deny row not effective for s1")
	}
	// temp 授权生效（目标入 sid 名单）
	if _, err := c.grantTarget("s1", "net", "example.com:443", false); err != nil {
		t.Fatal(err)
	}
	if !c.perms.netSnapshot("s1").Match("example.com:443") {
		t.Fatal("temp grant not effective for s1")
	}
	if c.perms.netSnapshot("s2").Match("example.com:443") {
		t.Fatal("temp grant leaked across sessions")
	}
	// 域路由：ssh 目标入 ssh 域而非 net 域
	if _, err := c.grantTarget("s1", "ssh", "10.0.0.2:22", false); err != nil {
		t.Fatal(err)
	}
	if !c.perms.sshAllowed("s1", "10.0.0.2:22") {
		t.Fatal("ssh grant not in ssh domain")
	}
	if c.perms.netSnapshot("s1").Match("10.0.0.2:22") {
		t.Fatal("ssh grant leaked into net domain")
	}
}

func TestExecAllowedGatesBrowserAndCUA(t *testing.T) {
	withGlobal(t, func(o *cfg.Options) { o.ExecPolicy = cfg.PolicyDeny })
	c, _ := testClient(t)
	// 虚拟指令（browser/cua）按 exec 域规则门控；未授权拒绝。
	if c.execAllowed("s1", "mcp.browser") {
		t.Fatal("ungranted browser allowed")
	}
	// 会话级 grant cmd 授权即时生效（规则数据，不是注册动作）。
	c.perms.grantCmd("s1", "mcp.browser")
	if !c.execAllowed("s1", "mcp.browser") {
		t.Fatal("session grant not effective")
	}
	if c.execAllowed("s2", "mcp.browser") {
		t.Fatal("session grant leaked across sessions")
	}
	if c.execAllowed("s1", "mcp.cua") {
		t.Fatal("grant leaked across commands")
	}
}

// TestPersistGrantAppendsRuleRow：--permanent 把规则行追加到 <域>_rules 表尾
// （无独立 grants 键），幂等归一不产生重复行；保存成功即原子发布基表。
func TestPersistGrantAppendsRuleRow(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv("APPDATA", dir)
	withGlobal(t, nil)
	c, _ := testClient(t)
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
	// 原子发布：基表即时生效
	if !c.perms.netSnapshot("").Match("example.com:443") {
		t.Fatal("permanent grant not published")
	}
}

// TestPersistGrantInvalidCandidateKeepsBase：候选配置无效 → 不落盘不发布，
// 运行基表保持原样。
func TestPersistGrantInvalidCandidateKeepsBase(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv("APPDATA", dir)
	withGlobal(t, nil)
	c, _ := testClient(t)
	// 文件里已有无效授权（手工编辑损坏）：候选完整校验失败
	p, _ := cfg.Path()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("fs_policy: deny\nfs_rules: ['rw:**']\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.persistGrant("ssh", "10.0.0.9:22"); err == nil {
		t.Fatal("invalid candidate persisted")
	}
	if c.perms.sshAllowed("s1", "10.0.0.9:22") {
		t.Fatal("failed persist published partial rules")
	}
}

// TestGrantFSResultSandboxWarning：普通回执不变；沙箱内授不上时附加警告
// （带原因与处置），但不把「授权已生效」改成失败。
func TestGrantFSResultSandboxWarning(t *testing.T) {
	clean := grantFSResult(`C:\models\llama`, "session", nil)
	if clean != `granted fs write access: C:\models\llama (scope=session)` {
		t.Fatalf("clean grant message changed: %q", clean)
	}
	got := grantFSResult(`C:\models\llama`, "permanent", errors.New("Access is denied."))
	for _, want := range []string{
		`granted fs write access: C:\models\llama (scope=permanent)`,
		"warn:", "Access is denied.", "WRITE_DAC", "Modify", "setowner", "*S-1-5-11",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("warning missing %q:\n%s", want, got)
		}
	}
	// 处置命令必须是可直接复制的原生态路径（%q 会把反斜杠转义成 C:\\models\\llama）。
	if strings.Contains(got, "setowner") && !strings.Contains(got, `icacls "C:\models\llama" /setowner "`) {
		t.Fatalf("icacls hint must carry the raw path:\n%s", got)
	}
}

// TestGrantFSProbeFailureKeepsGrant：探测失败 ≠ 授权失败——目录属主是
// Administrators 时沙箱内授不上写，但进程内 fs 工具层仍然可用（用宿主令牌写，
// 不需要 WRITE_DAC），所以 grant 照常生效，只附带警告。
func TestGrantFSProbeFailureKeepsGrant(t *testing.T) {
	dir := t.TempDir()
	withGlobal(t, func(o *cfg.Options) { o.FsPolicy = cfg.PolicyDeny })
	c := &Client{perms: newTestPerms(t, "")}
	orig := sandboxWritableProbe
	defer func() { sandboxWritableProbe = orig }()
	sandboxWritableProbe = func(string) error { return errors.New("Access is denied.") }
	msg, err := c.grantFS("s1", dir, false)
	if err != nil {
		t.Fatalf("probe failure must not reject the grant: %v", err)
	}
	if !strings.Contains(msg, "warn:") {
		t.Fatalf("missing sandbox warning: %q", msg)
	}
	if d := c.perms.fsSnapshot("s1").Match(filepath.Join(dir, "x.txt"), vbox.OpWrite); !d.Allow {
		t.Fatal("grant not applied despite probe failure")
	}
}
