package execution

import (
	"context"
	"strings"
	"testing"

	"github.com/veypi/vsh/commands"
	gbfs "github.com/veypi/vsh/fs"
)

func TestMissingPathNeverCallsFileCommandResolver(t *testing.T) {
	t.Parallel()
	for _, missingPath := range []string{"/bin/ghost", "/usr/bin/ghost"} {
		t.Run(missingPath, func(t *testing.T) {
			resolverCalls := 0
			e, err := NewEngine(EngineConfig{
				NewSessionFS: func(context.Context, string) (gbfs.FileSystem, string, error) {
					return gbfs.NewMemory(), "/", nil
				},
				FileCommands: commands.FileCommandResolverFunc(func(context.Context, commands.FileCommandRequest) (commands.Command, error) {
					resolverCalls++
					return nil, nil
				}),
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := e.Registry().Register(commands.DefineCommand("ghost", func(context.Context, *commands.Invocation) error { return nil })); err != nil {
				t.Fatal(err)
			}
			result, err := e.Exec(context.Background(), ExecRequest{Script: missingPath})
			if err != nil || result.ExitCode != 127 || result.Stdout != "" || !strings.Contains(result.Stderr, "No such file") {
				t.Fatalf("removed command result=%+v, err=%v", result, err)
			}
			if resolverCalls != 0 {
				t.Fatalf("missing path reached file command resolver %d times", resolverCalls)
			}
		})
	}
}
