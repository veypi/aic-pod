package host

import (
	"context"
	"github.com/veypi/aic-pod/libs/execution"

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
	// managed git：ssh 传输类子命令由 pod 侧受管执行（授权在 ssh 域，不经
	// cmd 域——本地子命令 managedGit 返回 false，继续走下面的 cmd 闸）。
	// 注意：受管路径只按解析名触发，执行体恒为 trustedGitBinary（系统
	// git），resolvedPath 指向的同名伪造二进制不会被 pod 侧运行。
	if name == "git" && c.perms != nil {
		if handled, err := c.managedGit(ctx, inv); handled {
			return err
		}
	}
	if !c.execAllowed(execution.SessionFromContext(ctx), name) {
		return commands.Exitf(inv, 126, "%s: command denied by exec rules (grant cmd %s)", resolvedPath, name)
	}
	wd := vbox.HostPathToOS(inv.Cwd)
	code, err := c.procs.RunProcess(ctx, vbox.StartOptions{
		Exec:  append([]string{vbox.HostPathToOS(resolvedPath)}, inv.Args...),
		Argv0: inv.Argv0,
		Env:   HostEnvToOS(inv.Env), ReplaceEnv: true, Workdir: wd,
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
