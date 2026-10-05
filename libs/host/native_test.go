package host

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/vbox"
	"github.com/veypi/vsh/commands"
)

func TestNativeResolvedIdentityAndStdio(t *testing.T) {
	withGlobal(t, func(o *cfg.Options) { o.ExecPolicy = cfg.PolicyDeny })
	c := &Client{procs: vbox.NewManager(), perms: newTestPerms(t, "")}
	c.procs.SetNoSandbox(true)
	var out, errOut strings.Builder
	inv := &commands.Invocation{Stdout: &out, Stderr: &errOut, Stdin: strings.NewReader("payload")}
	err := c.nativeExec(context.Background(), "/denied/git", inv)
	if code, _ := commands.ExitCode(err); code != 126 || !strings.Contains(errOut.String(), "grant cmd git") {
		t.Fatalf("denial=%v stderr=%q", err, errOut.String())
	}
	cat, err := exec.LookPath("cat")
	if err != nil {
		t.Skip(err)
	}
	// 基表更新 = 编译 + 原子发布（运行中改 cfg.Global 不生效）
	cfg.Global.ExecRules = []string{"allow:cat"}
	if err := c.nativeExec(context.Background(), cat, inv); err == nil {
		t.Fatal("runtime cfg.Global mutation leaked into permission state")
	}
	publishGlobal(t, c.perms)
	if err = c.nativeExec(context.Background(), cat, inv); err != nil || out.String() != "payload" {
		t.Fatalf("stdio=%q err=%v", out.String(), err)
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip(err)
	}
	cfg.Global.ExecRules = []string{"allow:sh"}
	publishGlobal(t, c.perms)
	out.Reset()
	errOut.Reset()
	inv.Args = []string{"-c", "echo out; echo err >&2; exit 7"}
	err = c.nativeExec(context.Background(), sh, inv)
	if code, _ := commands.ExitCode(err); code != 7 || out.String() != "out\n" || errOut.String() != "err\n" {
		t.Fatalf("code=%d out=%q err=%q failure=%v", code, out.String(), errOut.String(), err)
	}
	t.Setenv("AIC_INHERITED_PROBE", "must-not-leak")
	out.Reset()
	errOut.Reset()
	alias := "display-only"
	inv.Argv0 = &alias
	inv.Args = []string{"-c", `printf '%s|%s' "$0" "$AIC_INHERITED_PROBE"`}
	if err := c.nativeExec(context.Background(), sh, inv); err != nil || out.String() != "display-only|" {
		t.Fatalf("argv0/env=%q, err=%v", out.String(), err)
	}
	cfg.Global.ExecRules = []string{"allow:display-only"}
	publishGlobal(t, c.perms)
	if err := c.nativeExec(context.Background(), sh, inv); err == nil {
		t.Fatal("argv0 bypassed resolved identity")
	}

}
