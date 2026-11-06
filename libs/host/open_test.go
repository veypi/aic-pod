package host

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/execution"

	"github.com/veypi/vsh/commands"
	"github.com/veypi/vsh/policy"
)

func openTestInvocation(t *testing.T, workdir string, args []string, stdin string) (*commands.Invocation, *strings.Builder) {
	t.Helper()
	fsys, err := execution.NewHostFS(execution.HostFSConfig{Backing: OSVFS{}, WorkingDir: hostCanonical(workdir)})
	if err != nil {
		t.Fatal(err)
	}
	out := &strings.Builder{}
	return commands.NewInvocation(&commands.InvocationOptions{
		Args: args, Cwd: hostCanonical(workdir), Stdin: strings.NewReader(stdin),
		Stdout: out, Stderr: &strings.Builder{}, FileSystem: fsys, Policy: policy.NewStatic(nil),
	}), out
}

func TestOpenResolvesNativePathAndSupportsStdin(t *testing.T) {
	workdir := t.TempDir()
	name := filepath.Join(workdir, "a b.docx")
	if err := os.WriteFile(name, []byte("doc"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, _ := testClient(t)
	var opened []string
	c.openRun = func(_ context.Context, target string) error {
		opened = append(opened, target)
		return nil
	}
	inv, out := openTestInvocation(t, workdir, []string{"a b.docx"}, "")
	if err := c.runOpen(context.Background(), inv); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "opened ") {
		t.Fatalf("output = %q", out.String())
	}
	inv, _ = openTestInvocation(t, workdir, []string{"--stdin"}, name)
	if err := c.runOpen(context.Background(), inv); err != nil {
		t.Fatal(err)
	}
	if len(opened) != 2 || opened[0] != name || opened[1] != name {
		t.Fatalf("opened = %#v, want %q twice", opened, name)
	}
}

func TestOpenRejectsUsageMissingAndDeniedPaths(t *testing.T) {
	workdir := t.TempDir()
	name := filepath.Join(workdir, "secret.txt")
	if err := os.WriteFile(name, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	withGlobal(t, func(o *cfg.Options) { o.FsRules = []string{"deny:" + hostCanonical(workdir)} })
	c := New(Options{WorkDir: workdir})
	t.Cleanup(func() { c.Close() })
	called := false
	c.openRun = func(context.Context, string) error { called = true; return nil }
	inv, _ := openTestInvocation(t, workdir, nil, "")
	if err := c.runOpen(context.Background(), inv); err == nil {
		t.Fatal("missing path should fail")
	}
	inv, _ = openTestInvocation(t, workdir, []string{name}, "")
	if err := c.runOpen(context.Background(), inv); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("denied open error = %v", err)
	}
	if called {
		t.Fatal("native opener ran for invalid or denied input")
	}
}

func TestNativeOpenSpecUsesDirectSystemOpeners(t *testing.T) {
	target := `/tmp/a b$(false).docx`
	for _, tc := range []struct {
		goos, name string
		args       []string
	}{
		{"darwin", "/usr/bin/open", []string{target}},
		{"windows", "rundll32.exe", []string{"url.dll,FileProtocolHandler", target}},
		{"linux", "xdg-open", []string{target}},
	} {
		name, args, err := nativeOpenSpec(tc.goos, target)
		if err != nil || name != tc.name || len(args) != len(tc.args) {
			t.Fatalf("%s: name=%q args=%#v err=%v", tc.goos, name, args, err)
		}
		for i := range args {
			if args[i] != tc.args[i] {
				t.Fatalf("%s: args=%#v", tc.goos, args)
			}
		}
	}
	if _, _, err := nativeOpenSpec("plan9", target); err == nil {
		t.Fatal("unsupported OS should fail")
	}
}
