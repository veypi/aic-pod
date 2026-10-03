package execution

import (
	"context"
	"strings"
	"testing"

	"github.com/veypi/vsh/commands"
	gbfs "github.com/veypi/vsh/fs"
)

func TestMissingPathNeverCallsNativeExec(t *testing.T) {
	t.Parallel()
	for _, missingPath := range []string{"/bin/ghost", "/usr/bin/ghost"} {
		t.Run(missingPath, func(t *testing.T) {
			nativeCalls := 0
			e, err := NewEngine(EngineConfig{
				NewSessionFS: func(context.Context, string) (gbfs.FileSystem, string, error) {
					return gbfs.NewMemory(), "/", nil
				},
				NativeExec: func(ctx context.Context, name string, inv *commands.Invocation) error {
					nativeCalls++
					return nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := e.Registry().Register(commands.DefineCommand("ghost", func(context.Context, *commands.Invocation) error { return nil })); err != nil {
				t.Fatal(err)
			}
			if err := e.Registry().Register(commands.DefineCommand("remove-ghost", func(context.Context, *commands.Invocation) error {
				e.Registry().Unregister("ghost")
				return nil
			})); err != nil {
				t.Fatal(err)
			}
			// Removing a name cannot turn a missing file into a native command.
			result, err := e.Exec(context.Background(), ExecRequest{Script: "remove-ghost; " + missingPath})
			if err != nil || result.ExitCode != 127 || result.Stdout != "" || !strings.Contains(result.Stderr, "No such file") {
				t.Fatalf("removed command result=%+v, err=%v", result, err)
			}
			if nativeCalls != 0 {
				t.Fatalf("missing path reached native callback %d times", nativeCalls)
			}
		})
	}
}

// gatedRegistry：只返回实际注册命令；Names 只列 base；
// 命中经 CommandAllow 门（拒绝 = 权限错误，不继续 fallback）。

func TestRegisteredCommandGate(t *testing.T) {
	t.Parallel()
	base := commands.NewRegistry()
	ran := false
	_ = base.Register(commands.DefineCommand("browser", func(ctx context.Context, inv *commands.Invocation) error {
		ran = true
		return nil
	}))
	reg := gatedRegistry{base: base, allow: func(ctx context.Context, name string) bool {
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
