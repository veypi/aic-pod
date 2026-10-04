package host

import (
	"context"
	"github.com/veypi/aic-pod/libs/execution"
	"github.com/veypi/aic-pod/libs/proto"
	"github.com/veypi/vbox"
	"github.com/veypi/vsh/commands"
	"path"
	"runtime"
	"strings"
)

func (c *Client) nativeExec(ctx context.Context, resolvedPath string, inv *commands.Invocation) error {
	name := path.Base(resolvedPath)
	if runtime.GOOS == "windows" {
		name = strings.ToLower(name)
		for _, ext := range []string{".exe", ".com", ".bat", ".cmd"} {
			name = strings.TrimSuffix(name, ext)
		}
	}
	if reservedSSHNative(name) {
		return commands.Exitf(inv, 126, "%s: native SSH command is reserved; use managed ssh/scp/sftp and grant ssh host:port", resolvedPath)
	}
	if !c.execAllowed(execution.SessionFromContext(ctx), name) {
		return commands.Exitf(inv, 126, "%s: command denied by exec rules (grant cmd %s)", resolvedPath, name)
	}
	wd := proto.HostPathToOS(inv.Cwd)
	code, err := c.procs.RunProcess(ctx, vbox.StartOptions{
		Exec:  append([]string{proto.HostPathToOS(resolvedPath)}, inv.Args...),
		Argv0: inv.Argv0,
		Env:   proto.HostEnvToOS(inv.Env), ReplaceEnv: true, Workdir: wd,
		NoSandbox: execution.NoSandboxFromContext(ctx), Policy: c.nativePolicy(ctx, wd, name),
	}, inv.Stdin, inv.Stdout, inv.Stderr)
	if err != nil {
		return commands.Exitf(inv, 126, "%s: %v", resolvedPath, err)
	}
	if code != 0 {
		return &commands.ExitError{Code: code}
	}
	return nil
}

func reservedSSHNative(name string) bool {
	switch name {
	case "ssh", "scp", "sftp", "slogin", "ssh-copy-id", "ssh-keyscan":
		return true
	}
	return false
}
