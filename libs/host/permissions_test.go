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

// newTestPerms 以当前 cfg.Global 授权字段构建权限状态（隔离 HOME）。
func newTestPerms(t *testing.T, workDir string) *permissionState {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	p, err := newPermissionState(workDir, cfg.Global)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// publishGlobal 用当前 cfg.Global 重新编译并发布基表（permanent grant
// 发布路径的测试等效：编译失败即不发布）。
func publishGlobal(t *testing.T, p *permissionState) {
	t.Helper()
	base, err := compileAuth(cfg.AuthFrom(cfg.Global))
	if err != nil {
		t.Fatal(err)
	}
	p.publish(base)
}

func withGlobal(t *testing.T, mutate func(*cfg.Options)) {
	t.Helper()
	saved := cfg.Global
	cfg.Global = cfg.NewOptions()
	t.Cleanup(func() { cfg.Global = saved })
	if mutate != nil {
		mutate(cfg.Global)
	}
}

func TestPermsGrantFSIsolationAndDenyOverride(t *testing.T) {
	withGlobal(t, func(o *cfg.Options) {
		o.FsPolicy = cfg.PolicyDeny
		o.FsRules = []string{"deny:/secret/**"}
	})
	p := newTestPerms(t, t.TempDir())
	if d := p.fsSnapshot("s1").Match("/secret/x", vbox.OpWrite); d.Allow {
		t.Fatal("deny row not effective")
	}
	// temp 行插表头压一切（拒批已废：用户点就点了）
	p.grantFS("s1", "/secret")
	if d := p.fsSnapshot("s1").Match("/secret/x", vbox.OpWrite); !d.Allow {
		t.Fatal("temp grant over deny row not effective for s1")
	}
	if d := p.fsSnapshot("s2").Match("/secret/x", vbox.OpWrite); d.Allow {
		t.Fatal("temp grant leaked across sessions")
	}
	// 规则类目次序：temp → cfg → builtin deny → 便利根
	rows := p.fsSnapshot("s1").Rules
	if len(rows) == 0 || rows[0].Class != vbox.ClassTemp {
		t.Fatalf("temp grant must be first: %+v", rows[0])
	}
	var sawCfg, sawBuiltin, sawConvenience bool
	for _, r := range rows {
		switch r.Class {
		case vbox.ClassCfg:
			sawCfg = true
		case vbox.ClassBuiltin:
			sawBuiltin = true
		case vbox.ClassConvenience:
			sawConvenience = true
		}
	}
	if !sawCfg || !sawBuiltin || !sawConvenience {
		t.Fatalf("snapshot missing rule classes: %v", rows)
	}
	// 内建 deny 覆盖凭证路径
	if d := p.fsSnapshot("s1").Match(filepath.Join(os.Getenv("HOME"), ".ssh", "id_rsa"), vbox.OpRead); d.Allow {
		t.Fatal("builtin deny did not cover ~/.ssh")
	}
}

func TestPermsOpenModeFallback(t *testing.T) {
	withGlobal(t, func(o *cfg.Options) { o.FsPolicy = cfg.PolicyOpen })
	p := newTestPerms(t, t.TempDir())
	if d := p.fsSnapshot("s1").Match("/anywhere/file", vbox.OpWrite); !d.Allow {
		t.Fatal("open mode write denied")
	}
}

func TestPermsExecAllowedPublishAndGrants(t *testing.T) {
	withGlobal(t, func(o *cfg.Options) { o.ExecPolicy = cfg.PolicyDeny })
	p := newTestPerms(t, "")
	if p.execAllowed("s1", "git") {
		t.Fatal("deny policy admitted unlisted command")
	}
	cfg.Global.ExecRules = []string{"allow:git"}
	publishGlobal(t, p)
	if !p.execAllowed("s1", "git") {
		t.Fatal("published allow not effective")
	}
	cfg.Global.ExecRules = []string{"deny:git", "allow:git"}
	publishGlobal(t, p)
	if p.execAllowed("s1", "git") {
		t.Fatal("first deny did not revoke permission")
	}
	// 会话 temp 授权优先且隔离
	p.grantCmd("s1", "git")
	if !p.execAllowed("s1", "git") || p.execAllowed("s2", "git") {
		t.Fatal("session grant precedence or isolation broken")
	}
	if p.execAllowed("s1", "git;rm") {
		t.Fatal("invalid command name admitted")
	}
	cfg.Global.ExecRules = nil
	publishGlobal(t, p)
	if p.execAllowed("s2", "git") {
		t.Fatal("removed allow remained after publish")
	}
	if !p.execAllowed("s1", "git") {
		t.Fatal("session grant must survive base publish")
	}
}

// 启动配置无效：工具 fail-closed；发布有效基表（修复后重启的等效）恢复。
func TestPermsInvalidStartupFailsClosed(t *testing.T) {
	withGlobal(t, func(o *cfg.Options) {
		o.FsRules = []string{"rw:**"} // 全域模式被拒
	})
	p := newTestPerms(t, "")
	if p.err() == nil {
		t.Fatal("invalid startup config not recorded")
	}
	if p.execAllowed("s1", "git") {
		t.Fatal("invalid config admitted command")
	}
	if p.sshAllowed("s1", "example.com:22") {
		t.Fatal("invalid config admitted ssh")
	}
	// 快照自身 fail-closed：便利根也不放行（不依赖入口各自查 err）。
	rs := p.fsSnapshot("s1")
	if len(rs.Rules) != 0 || rs.DefaultWrite != vbox.EffDeny {
		t.Fatalf("invalid config snapshot not empty-deny: %d rules, default %v", len(rs.Rules), rs.DefaultWrite)
	}
	if d := rs.Match(filepath.ToSlash(os.TempDir())+"/x", vbox.OpWrite); d.Allow {
		t.Fatal("invalid config snapshot admitted temp-root write")
	}
	withGlobal(t, nil)
	publishGlobal(t, p)
	if p.err() != nil {
		t.Fatal("valid publish did not clear invalid state")
	}
}

// cmd 域会话授权去重（fs/net 同口径）。
func TestPermsGrantCmdDedup(t *testing.T) {
	p := newTestPerms(t, "")
	p.grantCmd("s1", "python3")
	p.grantCmd("s1", "python3")
	p.grantCmd("s1", "git")
	if n := len(p.grants["s1"].cmd); n != 2 {
		t.Fatalf("dup cmd grants recorded: %d", n)
	}
}

// 候选编译失败不发布：基表保持原样（permanent grant 原子性）。
func TestPermsCompileFailureKeepsBase(t *testing.T) {
	withGlobal(t, func(o *cfg.Options) { o.ExecPolicy = cfg.PolicyDeny })
	p := newTestPerms(t, "")
	cfg.Global.ExecRules = []string{"allow:git"}
	publishGlobal(t, p)
	if _, err := compileAuth(cfg.AuthFrom(&cfg.Options{FsRules: []string{"rw:**"}})); err == nil {
		t.Fatal("malformed candidate compiled")
	}
	if !p.execAllowed("s1", "git") {
		t.Fatal("failed compile disturbed the live base")
	}
}

func TestPermsNetSSHGrantsAndBuiltin(t *testing.T) {
	withGlobal(t, func(o *cfg.Options) {
		o.NetPolicy = cfg.PolicyDeny
		o.SshPolicy = cfg.PolicyDeny
	})
	p := newTestPerms(t, "")
	// net 内建 localhost:* 常驻表尾
	if d := p.netSnapshot("").Match("localhost:8080"); !d {
		t.Fatal("builtin localhost allow missing")
	}
	if d := p.netSnapshot("").Match("example.com:443"); d {
		t.Fatal("deny policy admitted net target")
	}
	e, err := vbox.ParseEntry("example.com:443")
	if err != nil {
		t.Fatal(err)
	}
	p.grantNet("s1", e, false)
	if !p.netSnapshot("s1").Match("example.com:443") || p.netSnapshot("s2").Match("example.com:443") {
		t.Fatal("net grant effectiveness or isolation broken")
	}
	se, err := vbox.ParseEntry("10.0.0.2:22")
	if err != nil {
		t.Fatal(err)
	}
	p.grantNet("s1", se, true)
	if !p.sshAllowed("s1", "10.0.0.2:22") {
		t.Fatal("ssh grant not in ssh domain")
	}
	if p.netSnapshot("s1").Match("10.0.0.2:22") {
		t.Fatal("ssh grant leaked into net domain")
	}
}

// 工作区元数据保护：.git/.aws 存在即 ro（git 命令豁免）。
func TestPermsSnapshotForNativeMetadata(t *testing.T) {
	withGlobal(t, func(o *cfg.Options) { o.FsPolicy = cfg.PolicyOpen })
	work := t.TempDir()
	if err := os.MkdirAll(filepath.Join(work, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := newTestPerms(t, work)
	target := filepath.ToSlash(filepath.Join(work, ".git", "config"))
	if d := p.fsSnapshotForNative("s1", work, "cp").Match(target, vbox.OpWrite); d.Allow {
		t.Fatal(".git not protected for non-git command")
	}
	if d := p.fsSnapshotForNative("s1", work, "git").Match(target, vbox.OpWrite); !d.Allow {
		t.Fatal("git command lost .git write")
	}
}

func TestPermsGrantStatus(t *testing.T) {
	withGlobal(t, func(o *cfg.Options) { o.ExecPolicy = cfg.PolicyDeny })
	p := newTestPerms(t, t.TempDir())
	p.grantCmd("s1", "git")
	out := p.grantStatus("s1")
	for _, want := range []string{"exec_policy: deny", "fs_policy:", "net_policy:", "ssh_policy:", "session cmd grants (1, 重启失效): git", "allow:localhost:*"} {
		if !strings.Contains(out, want) {
			t.Fatalf("grant status missing %q:\n%s", want, out)
		}
	}
}

// TestPermsGrantStatusMarksUngrantableWriteRoot：grant status 标注在 OS 沙箱内
// 授不上写权限的 rw 行（降级不静默——另一条渠道是 vbox 的 write-root 日志）。
func TestPermsGrantStatusMarksUngrantableWriteRoot(t *testing.T) {
	withGlobal(t, func(o *cfg.Options) {
		o.FsPolicy = cfg.PolicyDeny
		o.FsRules = []string{"rw:/opt/bad", "rw:/data/*/keep"}
	})
	p := newTestPerms(t, t.TempDir())
	orig := sandboxWritableProbe
	defer func() { sandboxWritableProbe = orig }()
	probed := map[string]int{}
	sandboxWritableProbe = func(abs string) error {
		probed[abs]++
		if abs == "/opt/bad" {
			return errors.New("Access is denied.")
		}
		return nil
	}
	out := p.grantStatus("s1")
	if !strings.Contains(out, "rw:/opt/bad [cfg] [沙箱内不可写: Access is denied.]") {
		t.Fatalf("grant status missing sandbox marker:\n%s", out)
	}
	if probed["/opt/bad"] != 1 {
		t.Fatalf("probe calls for /opt/bad = %d, want 1", probed["/opt/bad"])
	}
	// 含通配的 rw 行不进 OS 层可写根：不探测、不标注。
	if probed["/data/*/keep"] != 0 {
		t.Fatalf("glob row must not be probed: %v", probed)
	}
	if strings.Contains(out, "rw:/data/*/keep [cfg] [") {
		t.Fatalf("glob row must not be annotated:\n%s", out)
	}
}

// TestWriteRootProbeCacheLimit：探测缓存的三条约定——同一路径只探一次、通配与非
// rw 行不探、总路径数有上限（超出的记进 skippedNote，由 grant status 汇总说明）。
// 缓存已移出权限读锁：探测的等待不挡并发的 grant/publish。
func TestWriteRootProbeCacheLimit(t *testing.T) {
	origProbe, origLimit := sandboxWritableProbe, writeRootProbeLimit
	defer func() { sandboxWritableProbe = origProbe; writeRootProbeLimit = origLimit }()
	writeRootProbeLimit = 2
	var probed []string
	sandboxWritableProbe = func(abs string) error { probed = append(probed, abs); return nil }
	c := newWriteRootProbeCache()
	for _, row := range []vbox.Rule{
		{Pattern: "/opt/a", Effect: vbox.EffRW},
		{Pattern: "/opt/a", Effect: vbox.EffRW}, // 同一路径：只探一次
		{Pattern: "/opt/b", Effect: vbox.EffRW},
		{Pattern: "/opt/c", Effect: vbox.EffRW},      // 超上限：不探，只记 skipped
		{Pattern: "/opt/d/**", Effect: vbox.EffRW},   // 剥 /** 后无通配：算可写根，同样超上限
		{Pattern: "/opt/*/keep", Effect: vbox.EffRW}, // 真正的通配：永不成为可写根
		{Pattern: "/opt/ro", Effect: vbox.EffRO},
	} {
		_ = c.note(row)
	}
	if len(probed) != 2 {
		t.Fatalf("probe limit not honored: %v", probed)
	}
	note := c.skippedNote()
	if !strings.Contains(note, "另有 2 条") || !strings.Contains(note, "单次上限 2") {
		t.Fatalf("skipped note missing or mismatched: %q", note)
	}
	if empty := newWriteRootProbeCache().skippedNote(); empty != "" {
		t.Fatalf("empty cache must not emit a note: %q", empty)
	}
}
