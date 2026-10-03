package skillrun

import (
	"context"
	"path/filepath"

	"github.com/veypi/aic-pod/libs/execution"
	"github.com/veypi/aic-pod/libs/proto"
	"github.com/veypi/vbox"
	"github.com/veypi/vsh/commands"
)

// Resolve by name for every call so retained Command handles cannot invoke an old package.
func (r *Registry) rootCommand(name string) commands.Command {
	return commands.DefineCommand(name, func(ctx context.Context, inv *commands.Invocation) error {
		pkg, release, err := r.begin(name)
		if err != nil {
			return commands.Exitf(inv, 126, "%s", err)
		}
		defer release()
		if pkg.Manifest == nil {
			return commands.Exitf(inv, 126, "%s: no CLI", name)
		}
		if pkg.Manifest.Kind == KindProcess {
			return r.runProcess(ctx, inv, pkg)
		}
		return r.invokeService(ctx, inv, pkg)
	})
}

func (r *Registry) runProcess(ctx context.Context, inv *commands.Invocation, pkg *Package) error {
	p := pkg.Manifest
	entry := filepath.Join(pkg.Dir, filepath.FromSlash(p.Entry))
	workdir := inv.Cwd
	if r.deps.Workdir != nil {
		workdir = r.deps.Workdir(inv.Cwd)
	}
	var pol vbox.Policy
	if r.deps.Policy != nil {
		pol = r.deps.Policy(ctx, workdir, pkg.Name)
	}
	argv := append([]string{entry}, p.Args...)
	argv = append(argv, inv.Args...)
	code, err := r.deps.Manager.RunProcess(ctx, vbox.StartOptions{
		Workdir:    workdir,
		Exec:       argv,
		Argv0:      inv.Argv0,
		Env:        proto.HostEnvToOS(inv.Env),
		ReplaceEnv: true,
		NoSandbox:  execution.NoSandboxFromContext(ctx),
		Policy:     pol,
	}, inv.Stdin, inv.Stdout, inv.Stderr)
	if err != nil {
		return commands.Exitf(inv, exitCodeOr(code, 1), "%s: %s", pkg.Name, err)
	}
	if code != 0 {
		return &commands.ExitError{Code: code}
	}
	return nil
}

// exitCodeOr 进程错误时的 exit code 兜底（与 native.go 同语义）。
func exitCodeOr(code, fallback int) int {
	if code != 0 {
		return code
	}
	return fallback
}
