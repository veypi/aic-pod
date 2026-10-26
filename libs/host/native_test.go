package host

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/vsh/commands"
	vshpolicy "github.com/veypi/vsh/policy"
)

func TestRunFileCommandStdioAndExitStatus(t *testing.T) {
	c, _ := testClient(t)
	cat, err := exec.LookPath("cat")
	if err != nil {
		t.Skip(err)
	}
	var out, errOut strings.Builder
	inv := &commands.Invocation{Stdout: &out, Stderr: &errOut, Stdin: strings.NewReader("payload")}
	if err := c.runFileCommand(context.Background(), cat, inv); err != nil || out.String() != "payload" {
		t.Fatalf("cat stdout=%q stderr=%q err=%v", out.String(), errOut.String(), err)
	}

	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip(err)
	}
	out.Reset()
	errOut.Reset()
	inv.Args = []string{"-c", "echo out; echo err >&2; exit 7"}
	err = c.runFileCommand(context.Background(), sh, inv)
	if code, _ := commands.ExitCode(err); code != 7 || out.String() != "out\n" || errOut.String() != "err\n" {
		t.Fatalf("code=%d out=%q err=%q failure=%v", code, out.String(), errOut.String(), err)
	}
}

func TestRunFileCommandArgv0IsPresentationOnly(t *testing.T) {
	c, _ := testClient(t)
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip(err)
	}
	alias := "display-only"
	var out strings.Builder
	inv := &commands.Invocation{
		Stdout: &out,
		Argv0:  &alias,
		Args:   []string{"-c", `printf '%s' "$0"`},
	}
	if err := c.runFileCommand(context.Background(), sh, inv); err != nil || out.String() != alias {
		t.Fatalf("argv0 output=%q err=%v", out.String(), err)
	}
}

func TestNativeCommandNameSeparatesBareAndExplicitPath(t *testing.T) {
	if got := nativeCommandName("/usr/bin/git", "git"); got != "git" {
		t.Fatalf("bare name=%q", got)
	}
	if got := nativeCommandName("/usr/bin/git", "/usr/bin/git"); got != "/usr/bin/git" {
		t.Fatalf("explicit name=%q", got)
	}
	if got := nativeCommandName("/usr/bin/git", "./git"); got != "/usr/bin/git" {
		t.Fatalf("relative explicit name=%q", got)
	}
}

func TestAllowExecUsesNameAndResolvedPath(t *testing.T) {
	withGlobal(t, func(o *cfg.Options) {
		o.ExecPolicy = cfg.PolicyDeny
		o.ExecRules = []string{"allow:git"}
	})
	p := newTestPerms(t, "")
	if err := p.AllowExec(context.Background(), vshpolicy.ExecRequest{Name: "git", Path: "/usr/bin/git"}); err != nil {
		t.Fatalf("bare identity denied: %v", err)
	}
	if err := p.AllowExec(context.Background(), vshpolicy.ExecRequest{Name: "/usr/bin/git", Path: "/usr/bin/git"}); err == nil {
		t.Fatal("explicit path inherited the bare-name allow")
	}

	withGlobal(t, func(o *cfg.Options) {
		o.ExecPolicy = cfg.PolicyDeny
		o.ExecRules = []string{"allow:/usr/bin/git"}
	})
	p = newTestPerms(t, "")
	if err := p.AllowExec(context.Background(), vshpolicy.ExecRequest{Name: "git", Path: "/usr/bin/git"}); err != nil {
		t.Fatalf("resolved-path rule did not admit the binary: %v", err)
	}
}
