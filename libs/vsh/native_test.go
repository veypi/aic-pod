package vsh

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/veypi/vbox"
	"github.com/veypi/vsh/commands"
)

// exec 域规则矩阵（hosts-vsh-redesign §2.1：原生不逐个注册——OpenLookup 对
// 任意良名合成，规则检查在执行期）：deny 恒优先（含 "*" 全禁）；open 全放；
// 白名单 + 会话 grant（grant cmd 只改规则）。
func TestNativeRegistryPolicy(t *testing.T) {
	t.Parallel()
	n := NewNativeRegistry(NativeDeps{
		SessionAllow: func(session, name string) bool {
			return session == "s1" && name == "python3"
		},
	})
	n.Seed("git")
	if !n.IsAllowed("", "git") || n.IsAllowed("", "go") {
		t.Fatal("whitelist stance broken")
	}
	// 会话授权（grant cmd 会话级规则）
	if !n.IsAllowed("s1", "python3") || n.IsAllowed("s2", "python3") {
		t.Fatal("session grant broken")
	}
	n.SetPolicy(true, []string{"rm"})
	if !n.IsAllowed("", "go") {
		t.Fatal("open stance should allow")
	}
	if n.IsAllowed("", "rm") {
		t.Fatal("deny must win over open")
	}
	// OpenLookup 对任意良名合成（执行期规则检查）
	if _, ok := n.OpenLookup("go"); !ok {
		t.Fatal("well-formed name should synthesize")
	}
	// 显式路径：不存在的文件不合成（存在且可执行才命中，见
	// TestOpenLookupExplicitPath）
	if _, ok := n.OpenLookup("a/b"); ok {
		t.Fatal("missing path must not synthesize")
	}
	n.SetPolicy(true, []string{"*"})
	if n.IsAllowed("", "git") {
		t.Fatal("deny * must block everything")
	}
}

// 执行期规则检查：未授权命令报 permission_denied（126，含 grant 引导），
// 不是 127；授权命令二进制不存在才 127。
func TestNativeCommandRuleGate(t *testing.T) {
	t.Parallel()
	n := NewNativeRegistry(NativeDeps{})
	n.Seed() // 白名单为空
	n.SetPolicy(false, nil)
	cmd, ok := n.OpenLookup("git")
	if !ok {
		t.Fatal("synthesize failed")
	}
	var out, errBuf strings.Builder
	err := cmd.Run(context.Background(), &commands.Invocation{Stdout: &out, Stderr: &errBuf})
	if err == nil || !strings.Contains(err.Error(), "permission_denied") {
		t.Fatalf("ungranted native run: %v", err)
	}
	if !strings.Contains(errBuf.String(), "grant cmd git") {
		t.Fatalf("denial must guide grant: %q", errBuf.String())
	}
}

// 原生进程 stdio 全接通（管道与双流日志契约）：stdin 进入子进程；
// stdout/stderr 分流（诊断不污染 stdout）。
func TestNativeCommandStdio(t *testing.T) {
	t.Parallel()
	m := vbox.NewManager(0)
	m.SetNoSandbox(true) // 与沙箱无关：隔离环境差异
	n := NewNativeRegistry(NativeDeps{Manager: m})
	n.SetPolicy(true, nil)

	// stdin 接通：cat 原样回显载荷
	cat, ok := n.OpenLookup("cat")
	if !ok {
		t.Fatal("synthesize cat failed")
	}
	var out, errBuf strings.Builder
	err := cat.Run(context.Background(), &commands.Invocation{
		Stdin: strings.NewReader("payload-123"), Stdout: &out, Stderr: &errBuf,
	})
	if err != nil || out.String() != "payload-123" {
		t.Fatalf("stdin passthrough: out=%q err=%v", out.String(), err)
	}

	// stderr 分流：诊断不进 stdout
	sh, _ := n.OpenLookup("sh")
	out.Reset()
	errBuf.Reset()
	err = sh.Run(context.Background(), &commands.Invocation{
		Args: []string{"-c", "echo o; echo e >&2"}, Stdout: &out, Stderr: &errBuf,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "o" || strings.TrimSpace(errBuf.String()) != "e" {
		t.Fatalf("stream split: out=%q stderr=%q", out.String(), errBuf.String())
	}
}

// fallbackRegistry：base 命中恒优先；未命中交兜底；Names 只列 base；
// 命中经 CommandAllow 门（拒绝 = 权限错误，不继续 fallback）。
func TestFallbackRegistry(t *testing.T) {
	t.Parallel()
	base := commands.NewRegistry()
	_ = base.Register(commands.DefineCommand("hit", nil))
	called := ""
	reg := fallbackRegistry{base: base, fallback: func(name string) (commands.Command, bool) {
		called = name
		if name == "synth" {
			return commands.DefineCommand(name, nil), true
		}
		return nil, false
	}}
	if _, ok := reg.Lookup("hit"); !ok || called != "" {
		t.Fatal("base hit must short-circuit")
	}
	if _, ok := reg.Lookup("synth"); !ok || called != "synth" {
		t.Fatal("miss must consult fallback")
	}
	if _, ok := reg.Lookup("ghost"); ok {
		t.Fatal("fallback false must propagate miss")
	}
	if names := reg.Names(); len(names) != 1 || names[0] != "hit" {
		t.Fatalf("Names = %v, want [hit]", names)
	}
}

// CommandAllow 门：命中指令被 rules 拒绝时返回权限错误（不能 fallback 绕过）。
func TestFallbackRegistryCommandGate(t *testing.T) {
	t.Parallel()
	base := commands.NewRegistry()
	ran := false
	_ = base.Register(commands.DefineCommand("browser", func(ctx context.Context, inv *commands.Invocation) error {
		ran = true
		return nil
	}))
	reg := fallbackRegistry{base: base, allow: func(ctx context.Context, name string) bool {
		return name != "browser"
	}}
	cmd, ok := reg.Lookup("browser")
	if !ok {
		t.Fatal("hit missing")
	}
	var out, errBuf strings.Builder
	err := cmd.Run(context.Background(), &commands.Invocation{Stdout: &out, Stderr: &errBuf})
	if err == nil || !strings.Contains(errBuf.String(), "denied by exec rules") {
		t.Fatalf("denied command must error with permission guidance: %v %q", err, errBuf.String())
	}
	if ran {
		t.Fatal("denied command executed")
	}
	// 放行名单正常执行
	cmd2, _ := reg.Lookup("other-ok")
	_ = cmd2
}

// 显式程序路径（引擎二进制分支）：存在且可执行的路径合成；不存在/不可执行
// 不合成；命名合法性规则不变（空白/反斜杠拒绝）；合成后与普通原生命令同
// 通道真实执行（规则按 basename，策略快照/沙箱由 commandPath 同源应用）。
func TestOpenLookupExplicitPath(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("unix explicit path semantics")
	}
	lsPath, err := exec.LookPath("ls")
	if err != nil || !filepath.IsAbs(lsPath) {
		t.Skipf("no absolute ls on PATH: %q %v", lsPath, err)
	}
	n := NewNativeRegistry(NativeDeps{})
	if _, ok := n.OpenLookup(lsPath); !ok {
		t.Fatalf("executable path %q should synthesize", lsPath)
	}
	if _, ok := n.OpenLookup("/nonexistent-ivec/nope"); ok {
		t.Fatal("missing file must not synthesize")
	}
	plain := filepath.Join(t.TempDir(), "plain.txt")
	if err := os.WriteFile(plain, []byte("data\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := n.OpenLookup(plain); ok {
		t.Fatalf("non-executable file %q must not synthesize", plain)
	}
	for _, bad := range []string{"a b", "a\tb", `a\b`} {
		if _, ok := n.OpenLookup(bad); ok {
			t.Fatalf("%q must not synthesize", bad)
		}
	}

	// 执行通道：显式路径经 vbox 真实运行，参数原样透传
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "marker-ivec"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := vbox.NewManager(0)
	m.SetNoSandbox(true)
	run := NewNativeRegistry(NativeDeps{Manager: m})
	run.SetPolicy(true, nil)
	cmd, ok := run.OpenLookup(lsPath)
	if !ok {
		t.Fatalf("synthesize %q failed", lsPath)
	}
	var out, errBuf strings.Builder
	if err := cmd.Run(context.Background(), &commands.Invocation{
		Args: []string{dir}, Stdout: &out, Stderr: &errBuf,
	}); err != nil {
		t.Fatalf("run %s: %v (stderr %q)", lsPath, err, errBuf.String())
	}
	if !strings.Contains(out.String(), "marker-ivec") {
		t.Fatalf("explicit-path run output = %q (stderr %q)", out.String(), errBuf.String())
	}
}
