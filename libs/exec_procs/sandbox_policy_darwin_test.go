//go:build darwin

package exec_procs

import (
	"context"
	"github.com/veypi/aic-pod/libs/fsauth"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHostPolicyNativeEnforcement(t *testing.T) {
	if os.Getenv("AIC_SANDBOX_PROBE") != "1" {
		t.Skip("explicit native sandbox probe")
	}
	base := canonicalRoot(t.TempDir())
	rw, ro, out := filepath.Join(base, "rw"), filepath.Join(base, "ro"), filepath.Join(base, "outside")
	for _, dir := range []string{rw, ro, out} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "data"), []byte("original"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	deny := filepath.Join(rw, "blocked")
	if err := os.WriteFile(deny, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	read := []string{rw + "/**", ro + "/**"}
	for _, r := range fsauth.RuntimeReadRoots() {
		read = append(read, r+"/**")
	}
	run := func(script string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		plan, err := planConfined(confineSpec{level: 9, workdir: rw, extra: []string{rw}, readAllow: read, deny: []string{deny}, netOpen: true, argv: []string{"/bin/sh", "-c", script}})
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(ctx, plan.argv[0], plan.argv[1:]...)
		cmd.Dir = rw
		b, e := cmd.CombinedOutput()
		if e != nil {
			t.Logf("sandbox: %s", b)
		}
		return e
	}
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	if err := run("/bin/cat " + q(filepath.Join(ro, "data"))); err != nil {
		t.Fatalf("read-only read failed: %v", err)
	}
	if err := run("echo changed > " + q(filepath.Join(rw, "data"))); err != nil {
		t.Fatalf("allowed write failed: %v", err)
	}
	for _, script := range []string{"echo changed > " + q(filepath.Join(ro, "data")), "/bin/cat " + q(filepath.Join(out, "data")), "/bin/cat " + q(deny), "echo changed > " + q(deny)} {
		if err := run(script); err == nil {
			t.Fatalf("forbidden effect succeeded: %s", script)
		}
	}
	b, _ := os.ReadFile(filepath.Join(ro, "data"))
	if string(b) != "original" {
		t.Fatal("read-only file changed")
	}
	b, _ = os.ReadFile(deny)
	if string(b) != "secret" {
		t.Fatal("denied file changed")
	}
}

// TestHostPolicyReadScopeFromPolicy（darwin 原生探测）：读白名单直接来自
// Policy.ReadPatternsFor 时也必须收紧——空缓存根曾被展开成 "/**" 而使读锁
// 失效（2026-09-22 修复），该用例守住这条链路。
func TestHostPolicyReadScopeFromPolicy(t *testing.T) {
	if os.Getenv("AIC_SANDBOX_PROBE") != "1" {
		t.Skip("explicit native sandbox probe")
	}
	t.Setenv("GOCACHE", "")
	t.Setenv("XDG_CACHE_HOME", "")
	work := canonicalRoot(t.TempDir())
	inside := filepath.Join(work, "inside.txt")
	if err := os.WriteFile(inside, []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}
	outsideDir, err := os.MkdirTemp("/Users/Shared", "aic-readscope-")
	if err != nil {
		t.Skipf("no writable outside dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(outsideDir) })
	outside := filepath.Join(outsideDir, "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}

	pol := fsauth.New()
	pol.SetWorkDir(work)
	read := pol.ReadPatternsFor("")
	for _, pat := range read {
		if pat == "" || pat == "/**" {
			t.Fatalf("policy produced match-all read pattern: %#v", read)
		}
	}

	run := func(script string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		plan, err := planConfined(confineSpec{level: 9, workdir: work, extra: []string{work}, readAllow: read, deny: pol.DenyPatterns(), netOpen: true, argv: []string{"/bin/sh", "-c", script}})
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(ctx, plan.argv[0], plan.argv[1:]...)
		cmd.Dir = work
		b, e := cmd.CombinedOutput()
		if e != nil {
			t.Logf("sandbox: %s", b)
		}
		return e
	}
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	if err := run("/bin/cat " + q(inside)); err != nil {
		t.Fatalf("workdir read should succeed: %v", err)
	}
	if err := run("/bin/cat " + q(outside)); err == nil {
		t.Fatalf("read outside readAllow succeeded (read lockdown void): %s", outside)
	}
}

// TestHostPolicyLiteralSpellings（darwin 原生探测）：/etc、/tmp、$TMPDIR
// (/var/folders/…) 的 symlink 字面拼写必须与 canonical 形一样放行（读+写），
// 同时白名单外写仍被拒（2026-09-22）。
func TestHostPolicyLiteralSpellings(t *testing.T) {
	if os.Getenv("AIC_SANDBOX_PROBE") != "1" {
		t.Skip("explicit native sandbox probe")
	}
	work := canonicalRoot(t.TempDir())
	pol := fsauth.New()
	pol.SetWorkDir(work)
	tmp := strings.TrimSuffix(os.TempDir(), "/")
	tmpFile := filepath.Join(tmp, "aic-spelling-probe.txt")
	if err := os.WriteFile(tmpFile, []byte("tmp"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(tmpFile) })

	run := func(script string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		plan, err := planConfined(confineSpec{level: 9, workdir: work, extra: pol.WriteRootsFor(""), readAllow: pol.ReadPatternsFor(""), deny: pol.DenyPatterns(), netOpen: true, argv: []string{"/bin/sh", "-c", script}})
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(ctx, plan.argv[0], plan.argv[1:]...)
		cmd.Dir = work
		b, e := cmd.CombinedOutput()
		if e != nil {
			t.Logf("sandbox: %s", b)
		}
		return e
	}
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	if err := run("/bin/cat /etc/hosts"); err != nil {
		t.Fatalf("literal /etc read denied: %v", err)
	}
	if err := run("/bin/cat " + q(tmpFile)); err != nil {
		t.Fatalf("literal $TMPDIR read denied: %v", err)
	}
	if err := run("/usr/bin/touch /tmp/aic-spelling-ok"); err != nil {
		t.Fatalf("literal /tmp write denied: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove("/tmp/aic-spelling-ok") })
	if err := run("/usr/bin/touch " + q(filepath.Join(tmp, "aic-spelling-ok"))); err != nil {
		t.Fatalf("literal $TMPDIR write denied: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(filepath.Join(tmp, "aic-spelling-ok")) })
	if err := run("/usr/bin/touch /Users/Shared/aic-spelling-should-fail"); err == nil {
		t.Fatal("write outside allow-list succeeded")
	}
}

// TestHostPolicyXcodeShimTools（darwin 原生探测）：Xcode 命令行工具 shim
// （/usr/bin/git、python3、cc 先读 /var/db/xcode_select_link 解析 developer dir，
// 再去那里执行真实工具）必须在读白名单内可执行——2026-09-22 收紧读锁后缺该
// 读根，shim 报 "unable to read data link ... (Operation not permitted)"。
func TestHostPolicyXcodeShimTools(t *testing.T) {
	if os.Getenv("AIC_SANDBOX_PROBE") != "1" {
		t.Skip("explicit native sandbox probe")
	}
	work := canonicalRoot(t.TempDir())
	pol := fsauth.New()
	pol.SetWorkDir(work)
	run := func(script string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		plan, err := planConfined(confineSpec{level: 9, workdir: work, extra: pol.WriteRootsFor(""), readAllow: pol.ReadPatternsFor(""), deny: pol.DenyPatterns(), netOpen: true, argv: []string{"/bin/sh", "-c", script}})
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(ctx, plan.argv[0], plan.argv[1:]...)
		cmd.Dir = work
		b, e := cmd.CombinedOutput()
		if e != nil {
			t.Logf("sandbox: %s", b)
		}
		return e
	}
	if err := run("/usr/bin/xcode-select -p"); err != nil {
		t.Fatalf("xcode-select shim failed in sandbox: %v", err)
	}
	// 真实 git 还会读 ~/.gitconfig（用户数据，锁内不可读）而硬退；这里用空全局
	// 配置验证工具本体可达（沙箱内 git 若需读用户配置，由用户自行 ro: 放行）。
	if err := run("GIT_CONFIG_GLOBAL=/dev/null /usr/bin/git --version"); err != nil {
		t.Fatalf("xcode shim git denied in sandbox: %v", err)
	}
	if _, err := os.Stat("/usr/bin/python3"); err == nil {
		if err := run("/usr/bin/python3 --version"); err != nil {
			t.Fatalf("xcode shim python3 denied in sandbox: %v", err)
		}
	}
	if _, err := os.Stat("/usr/bin/cc"); err == nil {
		if err := run("/usr/bin/cc --version"); err != nil {
			t.Fatalf("xcode shim cc denied in sandbox: %v", err)
		}
	}
}
