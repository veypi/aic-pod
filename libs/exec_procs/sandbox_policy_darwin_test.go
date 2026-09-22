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
	run := func(script string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		plan, err := planConfined(confineSpec{level: 9, workdir: rw, extra: []string{rw}, deny: []string{deny}, netOpen: true, argv: []string{"/bin/sh", "-c", script}})
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
	// 读默认开放：白名单外目录（ro/outside）也可读。
	for _, script := range []string{
		"/bin/cat " + q(filepath.Join(ro, "data")),
		"/bin/cat " + q(filepath.Join(out, "data")),
	} {
		if err := run(script); err != nil {
			t.Fatalf("open read failed (%s): %v", script, err)
		}
	}
	if err := run("echo changed > " + q(filepath.Join(rw, "data"))); err != nil {
		t.Fatalf("allowed write failed: %v", err)
	}
	// 白名单外写、deny 读、deny 写均被拒。
	for _, script := range []string{
		"echo changed > " + q(filepath.Join(ro, "data")),
		"/bin/cat " + q(deny),
		"echo changed > " + q(deny),
	} {
		if err := run(script); err == nil {
			t.Fatalf("forbidden effect succeeded: %s", script)
		}
	}
	b, _ := os.ReadFile(filepath.Join(ro, "data"))
	if string(b) != "original" {
		t.Fatal("write outside allow-list changed file")
	}
	b, _ = os.ReadFile(deny)
	if string(b) != "secret" {
		t.Fatal("denied file changed")
	}
}

// TestHostPolicyReadOpenDenyScope（darwin 原生探测）：读默认开放（白名单外
// 路径可读），deny 仍是读写双拒。读白名单机制已随「读开放」删除，本用例守住
// 新模型两端（白名单外可读 + deny 生效）。
func TestHostPolicyReadOpenDenyScope(t *testing.T) {
	if os.Getenv("AIC_SANDBOX_PROBE") != "1" {
		t.Skip("explicit native sandbox probe")
	}
	work := canonicalRoot(t.TempDir())
	inside := filepath.Join(work, "inside.txt")
	if err := os.WriteFile(inside, []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 命中默认 deny 表（**/*.key）的目标：读/写都必须被拒。
	denied := filepath.Join(work, "secret.key")
	if err := os.WriteFile(denied, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	outsideDir, err := os.MkdirTemp("/Users/Shared", "aic-readopen-")
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
	run := func(script string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		plan, err := planConfined(confineSpec{level: 9, workdir: work, extra: []string{work}, deny: pol.DenyPatterns(), netOpen: true, argv: []string{"/bin/sh", "-c", script}})
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
		t.Fatalf("workdir read failed: %v", err)
	}
	if err := run("/bin/cat " + q(outside)); err != nil {
		t.Fatalf("read outside allow-list denied (read must be open): %v", err)
	}
	if err := run("/bin/cat " + q(denied)); err == nil {
		t.Fatalf("deny read succeeded: %s", denied)
	}
	if err := run("echo changed > " + q(denied)); err == nil {
		t.Fatalf("deny write succeeded: %s", denied)
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
		plan, err := planConfined(confineSpec{level: 9, workdir: work, extra: pol.WriteRootsFor(""), deny: pol.DenyPatterns(), netOpen: true, argv: []string{"/bin/sh", "-c", script}})
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
// 再去那里执行真实工具）必须在沙箱内可执行。读默认开放后 shim 读链不再受限，
// 本用例作为工具链在沙箱内可达的回归点保留。
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
		plan, err := planConfined(confineSpec{level: 9, workdir: work, extra: pol.WriteRootsFor(""), deny: pol.DenyPatterns(), netOpen: true, argv: []string{"/bin/sh", "-c", script}})
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
	// 真实 git 还会读 ~/.gitconfig（用户数据；默认 deny 表不含它，读开放后
	// 仍可读）；这里用空全局配置验证工具本体可达。
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
