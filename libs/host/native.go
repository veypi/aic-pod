package host

import (
	"context"
	"path"
	"runtime"
	"strings"

	"github.com/veypi/aic-pod/libs/execution"
	"github.com/veypi/aic-pod/libs/gitx"
	"github.com/veypi/aic-pod/libs/sshx"

	"github.com/veypi/vbox"
	"github.com/veypi/vsh/commands"
)

// resolveFileCommand constructs an ordinary vsh command for a resolved file.
// Admission has already been centralized in the engine; this resolver performs
// no SSH/Git/name-based interception and never executes during resolution.
func (c *Client) resolveFileCommand(_ context.Context, req commands.FileCommandRequest) (commands.Command, error) {
	name := nativeCommandName(req.Path, req.InvokedName)
	return commands.DefineCommand(name, func(ctx context.Context, inv *commands.Invocation) error {
		return c.runFileCommand(ctx, req.Path, inv)
	}), nil
}

// registerManagedGit explicitly registers the optional Pod git command. SSH
// transport belongs to gitx; other subcommands use the trusted system binary
// through the same vbox-backed implementation. Absolute native git never enters it.
func (c *Client) registerManagedGit(reg *commands.Registry) error {
	gitPath, err := gitx.TrustedBinary()
	if err != nil {
		return nil
	}
	return reg.RegisterGuarded(commands.DefineCommand("git", func(ctx context.Context, inv *commands.Invocation) error {
		handled, err := c.git.Managed(ctx, inv)
		if handled {
			return err
		}
		return c.runFileCommand(ctx, gitPath, inv)
	}))
}

func nativeCommandName(resolvedPath, invokedName string) string {
	if strings.Contains(invokedName, "/") {
		return resolvedPath
	}
	name := path.Base(resolvedPath)
	if runtime.GOOS == "windows" {
		name = strings.ToLower(name)
		for _, ext := range []string{".exe", ".com", ".bat", ".cmd"} {
			name = strings.TrimSuffix(name, ext)
		}
	}
	return name
}

func (c *Client) runFileCommand(ctx context.Context, resolvedPath string, inv *commands.Invocation) error {
	wd := vbox.HostPathToOS(inv.Cwd)
	code, err := c.procs.RunProcess(ctx, vbox.StartOptions{
		Exec:  append([]string{vbox.HostPathToOS(resolvedPath)}, inv.Args...),
		Argv0: inv.Argv0,
		Env:   HostEnvToOS(inv.Env), ReplaceEnv: true, Workdir: wd,
		NoSandbox: execution.NoSandboxFromContext(ctx), Policy: c.nativePolicy(ctx, wd, nativeCommandName(resolvedPath, "")),
	}, inv.Stdin, inv.Stdout, inv.Stderr)
	if err != nil {
		return commands.Exitf(inv, 126, "%s: %v", resolvedPath, err)
	}
	if code != 0 {
		return &commands.ExitError{Code: code}
	}
	return nil
}

// trustedSSHBinary 保留 host 侧测试入口（实现已迁 libs/sshx）。
func trustedSSHBinary() (string, error) { return sshx.TrustedBinary() }

// managedGit 保留 host 测试入口（实现已迁 libs/gitx）。
func (c *Client) managedGit(ctx context.Context, inv *commands.Invocation) (bool, error) {
	return c.git.Managed(ctx, inv)
}

// trustedGitBinary 保留 host 集成测试入口（实现已迁 libs/gitx）。
func trustedGitBinary() (string, error) { return gitx.TrustedBinary() }
