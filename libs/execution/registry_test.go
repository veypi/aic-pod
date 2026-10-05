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
			// A registered name elsewhere cannot turn a missing file into a native command.
			result, err := e.Exec(context.Background(), ExecRequest{Script: missingPath})
			if err != nil || result.ExitCode != 127 || result.Stdout != "" || !strings.Contains(result.Stderr, "No such file") {
				t.Fatalf("removed command result=%+v, err=%v", result, err)
			}
			if nativeCalls != 0 {
				t.Fatalf("missing path reached native callback %d times", nativeCalls)
			}
		})
	}
}

// skill 命令准入薄检查（原 gatedRegistry 的等效语义，2026-10-06 §4）：
// Allow 拒绝 = 126 + grant 引导，不继续执行。
func TestSkillCommandGate(t *testing.T) {
	t.Parallel()
	ran := false
	d := PlatformDeps{Skill: SkillDeps{
		Allow: func(context.Context) bool { return false },
		Load:  func(context.Context, string, string) (string, error) { ran = true; return "", nil },
	}}
	var out, errBuf strings.Builder
	err := d.cmdSkill(context.Background(), &commands.Invocation{Args: []string{"load", "x"}, Stdout: &out, Stderr: &errBuf})
	if code, _ := commands.ExitCode(err); code != 126 || !strings.Contains(errBuf.String(), "denied by exec rules") {
		t.Fatalf("denied skill must error with permission guidance: %v %q", err, errBuf.String())
	}
	if ran {
		t.Fatal("denied skill command executed")
	}
	// nil Allow = 放行（cloud）
	d.Skill.Allow = nil
	if err := d.cmdSkill(context.Background(), &commands.Invocation{Args: []string{"load", "x"}, Stdout: &out, Stderr: &errBuf}); err != nil || !ran {
		t.Fatalf("nil Allow must pass: %v ran=%v", err, ran)
	}
}
