package host

// net 规则平台能力闸的测试（2026-10-10，OpenShell 调研后的缺陷修复）：
//   - 配置期：既不可落实也不可吸收的行（linux/windows 的选择性 deny）在
//     compileAuth 拒绝，报文指向 tool: 前缀——不再等 exec 期暴雷；
//   - 被吸收的行（deny 姿态下的 no-op allow）不再拒启，由 grant status 逐条标注；
//   - tool: 行不进原生沙箱快照，工具层（curl/MCP）照常命中；
//   - grant 回执标注原生沙箱内的实际生效情况（对齐 grant fs 的可写性警告）。

import (
	"strings"
	"testing"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/vbox"
)

// pinNativeNetPlatform 注入原生沙箱能力校验的平台标识（与 CI 运行平台解耦）。
func pinNativeNetPlatform(t *testing.T, platform string) {
	t.Helper()
	saved := nativeNetPlatform
	nativeNetPlatform = platform
	t.Cleanup(func() { nativeNetPlatform = saved })
}

func TestCompileAuthNetPlatformGate(t *testing.T) {
	// 缺陷 B：linux 上选择性 deny 必须配置期报错，且报文指向 tool: 前缀。
	pinNativeNetPlatform(t, "linux")
	_, err := compileAuth(cfg.AuthFrom(&cfg.Options{NetRules: []string{"deny:169.254.169.254:*"}}))
	if err == nil || !strings.Contains(err.Error(), "tool:") {
		t.Fatalf("selective deny on linux must fail with tool: guidance, got %v", err)
	}
	// tool: 前缀后合法，且被吸收的内建行不影响编译。
	base, err := compileAuth(cfg.AuthFrom(&cfg.Options{NetRules: []string{"tool:deny:169.254.169.254:*"}}))
	if err != nil {
		t.Fatalf("tool-only deny must compile: %v", err)
	}
	if len(base.netRules) != 1 || !base.netRules[0].ToolOnly {
		t.Fatalf("tool-only flag lost in compile: %+v", base.netRules)
	}
	// 缺陷 A：linux + deny 姿态 + 内建 localhost allow 不再报错（吸收行的
	// 可见性由 grant status 标注承担，见 TestGrantStatusNetLandingNotes）。
	if _, err = compileAuth(cfg.AuthFrom(&cfg.Options{NetPolicy: cfg.PolicyDeny})); err != nil {
		t.Fatalf("deny posture must compile on linux: %v", err)
	}
	// windows 无网络能力：deny 姿态整体拒绝，且报文指向 open+tool: 组合
	//（姿态本身加不了 tool: 前缀）。
	pinNativeNetPlatform(t, "windows")
	if _, err := compileAuth(cfg.AuthFrom(&cfg.Options{NetPolicy: cfg.PolicyDeny})); err == nil ||
		!strings.Contains(err.Error(), "net_policy=open") {
		t.Fatalf("deny posture on windows must fail with open+tool: guidance, got %v", err)
	}
	// darwin + deny 姿态 + loopback allow 落实。
	pinNativeNetPlatform(t, "darwin")
	if _, err = compileAuth(cfg.AuthFrom(&cfg.Options{NetPolicy: cfg.PolicyDeny, NetRules: []string{"allow:localhost:8080"}})); err != nil {
		t.Fatal(err)
	}
	// darwin loopback 端口坍缩：同端口混合拼写、效果相反 → 配置期拒绝
	//（内核会比策略更宽；vbox 侧矩阵见 netpolicy_conformance_test.go）。
	if _, err = compileAuth(cfg.AuthFrom(&cfg.Options{
		NetPolicy: cfg.PolicyDeny,
		NetRules:  []string{"allow:127.0.0.1:8080", "deny:localhost:8080"},
	})); err == nil {
		t.Fatal("conflicting loopback rows on one port must fail compile on darwin")
	}
}

func TestNetSnapshotForNativeDropsToolOnly(t *testing.T) {
	pinNativeNetPlatform(t, "linux")
	withGlobal(t, func(o *cfg.Options) {
		o.NetRules = []string{"tool:deny:169.254.169.254:*", "allow:example.com:443"}
	})
	p := newTestPerms(t, "")
	// 工具层快照：tool-only 行照常命中（curl/MCP 口径）。
	if p.netSnapshot("s1").Match("169.254.169.254:443") {
		t.Fatal("tool-only deny not effective at tool layer")
	}
	// 原生快照：tool-only 行被剔除，其余保留。
	native := p.netSnapshotForNative("s1")
	for _, r := range native.Rules {
		if r.ToolOnly {
			t.Fatalf("tool-only row leaked into native snapshot: %v", r)
		}
	}
	if !native.Match("169.254.169.254:443") {
		t.Fatal("native snapshot default(open) should allow metadata target once tool-only row is dropped")
	}
	if len(native.Rules) != 2 { // allow:example.com:443 + 内建 localhost:*
		t.Fatalf("native rows = %v", native.Rules)
	}
	// 原生快照通过平台能力校验（缺陷 A/B 的回归闸：不再拒启）。
	if _, err := vbox.CheckNetPolicy(nativeNetPlatform, native); err != nil {
		t.Fatal(err)
	}
}

func TestGrantNetNativeNote(t *testing.T) {
	pinNativeNetPlatform(t, "darwin")
	withGlobal(t, func(o *cfg.Options) { o.NetPolicy = cfg.PolicyDeny })
	c := &Client{perms: newTestPerms(t, "")}
	// deny 姿态下 grant 非 loopback：授权生效（工具层），回执标注原生不生效。
	msg, err := c.grantTarget("s1", "net", "example.com:443", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "原生沙箱内不生效") {
		t.Fatalf("want native-ineffective note, got %q", msg)
	}
	if !c.perms.netSnapshot("s1").Match("example.com:443") {
		t.Fatal("grant not effective at tool layer")
	}
	// deny 姿态下 grant loopback（darwin 可落实）：无标注。
	msg, err = c.grantTarget("s1", "net", "localhost:8080", false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(msg, "不生效") {
		t.Fatalf("loopback grant lands on darwin, want no note, got %q", msg)
	}
	// ssh 域不标注（本就不进原生沙箱）。
	msg, err = c.grantTarget("s1", "ssh", "10.0.0.2:22", false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(msg, "note:") || strings.Contains(msg, "warn:") {
		t.Fatalf("ssh grant must not carry native note, got %q", msg)
	}
}

// TestGrantStatusNetLandingNotes：grant status 标注原生沙箱内的吸收行与
// tool-only 行。
func TestGrantStatusNetLandingNotes(t *testing.T) {
	pinNativeNetPlatform(t, "linux")
	withGlobal(t, func(o *cfg.Options) {
		o.NetPolicy = cfg.PolicyDeny
		o.NetRules = []string{"tool:deny:169.254.169.254:*"}
	})
	p := newTestPerms(t, "")
	status := p.grantStatus("s1")
	if !strings.Contains(status, "tool:deny:169.254.169.254:* [仅工具层生效]") {
		t.Fatalf("tool-only row not annotated: %s", status)
	}
	if !strings.Contains(status, "allow:localhost:* [原生沙箱内不可达") {
		t.Fatalf("absorbed builtin row not annotated: %s", status)
	}
}
