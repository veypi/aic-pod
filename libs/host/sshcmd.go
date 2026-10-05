package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"github.com/veypi/aic-pod/libs/execution"
	"github.com/veypi/vbox"
	"github.com/veypi/vsh/commands"
)

type sshProcessRunner func(context.Context, vbox.StartOptions, io.Reader, io.Writer, io.Writer) (int, error)

func (c *Client) sshOptions(ctx context.Context, raw string, port int) (vbox.StartOptions, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return vbox.StartOptions{}, err
	}
	t, err := resolveSSHTarget(raw, home, port)
	if err != nil {
		return vbox.StartOptions{}, err
	}
	if c.sshPol == nil || !c.sshPol.Allowed(execution.SessionFromContext(ctx), t.Host, t.Port) {
		return vbox.StartOptions{}, fmt.Errorf("%w: SSH %q resolves to %s (user %s); request grant ssh %s", fs.ErrPermission, raw, t.address(), t.User, t.address())
	}
	binary, err := trustedSSHBinary()
	if err != nil {
		return vbox.StartOptions{}, err
	}
	args := []string{binary, "-F", "none", "-T"}
	for _, option := range []string{
		"BatchMode=yes", "ConnectTimeout=10", "ConnectionAttempts=1", "StrictHostKeyChecking=yes",
		"ClearAllForwardings=yes", "ForwardAgent=no", "ForwardX11=no", "Tunnel=no",
		"ProxyCommand=none", "ProxyJump=none", "PermitLocalCommand=no", "KnownHostsCommand=none",
		"ControlMaster=no", "ControlPath=none", "ControlPersist=no", "ForkAfterAuthentication=no",
		"CanonicalizeHostname=no", "RemoteCommand=none", "RequestTTY=no", "EscapeChar=none",
		"PreferredAuthentications=publickey", "PasswordAuthentication=no", "KbdInteractiveAuthentication=no",
		"HostbasedAuthentication=no", "GSSAPIAuthentication=no", "AddKeysToAgent=no", "PKCS11Provider=none",
		"UpdateHostKeys=no", "VerifyHostKeyDNS=no", "CheckHostIP=no", "GlobalKnownHostsFile=none",
		"UserKnownHostsFile=" + sshOptionPath(filepath.Join(home, ".ssh", "known_hosts")),
	} {
		args = append(args, "-o", option)
	}
	if t.IdentitiesOnly {
		args = append(args, "-o", "IdentitiesOnly=yes")
	}
	for _, p := range t.Identities {
		args = append(args, "-i", strings.ReplaceAll(p, "%", "%%"))
	}
	args = append(args, "-p", strconv.Itoa(t.Port), "-l", t.User, "--", t.Host)
	return vbox.StartOptions{Exec: args, Env: sshClientEnv(home), ReplaceEnv: true, NoSandbox: true, RawOutput: true, Workdir: home}, nil
}

// sshEnvPassthrough 是唯一允许从 pod 设备环境透传给 OpenSSH 客户端的变量（不传脚本
// 环境，脚本变量一律靠 ReplaceEnv 隔绝）。Windows 必须带 ProgramData：Win32-OpenSSH
// 缺它会把自己所有诊断输出整个吞掉——连接失败仍是 exit 255，但 stdout/stderr 全空，
// 排查时看起来像“黑箱”（2026-10-05 win 实机：补上 ProgramData 即恢复
// "ssh: connect to host … Connection refused"；逐个候选变量二分得出）。
var sshEnvPassthrough = []string{
	"SSH_AUTH_SOCK", "LANG", "LC_ALL", "SYSTEMROOT", "WINDIR", "USERPROFILE", "ProgramData",
}

// sshClientEnv 构造客户端环境：HOME 固定为设备家目录，其余仅白名单透传。
func sshClientEnv(home string) []string {
	env := []string{"HOME=" + home, "SSH_ASKPASS_REQUIRE=never"}
	for _, key := range sshEnvPassthrough {
		if value := os.Getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}
	return env
}

func sshOptionPath(p string) string {
	p = strings.ReplaceAll(p, "%", "%%")
	p = strings.ReplaceAll(p, "\\", "\\\\")
	return `"` + strings.ReplaceAll(p, `"`, `\"`) + `"`
}

func trustedSSHBinary() (string, error) {
	candidates := []string{"/usr/bin/ssh", "/bin/ssh"}
	if runtime.GOOS == "windows" {
		candidates = []string{filepath.Join(os.Getenv("SYSTEMROOT"), "System32", "OpenSSH", "ssh.exe")}
	}
	for _, p := range candidates {
		if !filepath.IsAbs(p) {
			continue
		}
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			return p, nil
		}
	}
	return "", fmt.Errorf("system OpenSSH client is unavailable")
}

func (c *Client) runSSH(ctx context.Context, opts vbox.StartOptions, in io.Reader, out, errout io.Writer) (int, error) {
	if c.sshRun != nil {
		return c.sshRun(ctx, opts, in, out, errout)
	}
	return c.procs.RunProcess(ctx, opts, in, out, errout)
}

func sshHelp(inv *commands.Invocation, usage string) bool {
	if len(inv.Args) == 1 && (inv.Args[0] == "--help" || inv.Args[0] == "-h") {
		fmt.Fprintln(inv.Stdout, usage)
		return true
	}
	return false
}

const sshUsage = "usage: ssh <[user@]host[:port]|alias> <remote command...>\nManaged host SSH: public-key authentication, known_hosts required, no TTY/options/forwarding. Remote command arguments are joined with spaces; quote the entire remote shell command. Authorization: grant ssh host:port."

func (c *Client) managedSSH(ctx context.Context, inv *commands.Invocation) error {
	if sshHelp(inv, sshUsage) {
		return nil
	}
	if len(inv.Args) < 2 || strings.HasPrefix(inv.Args[0], "-") || strings.TrimSpace(strings.Join(inv.Args[1:], " ")) == "" {
		return commands.Exitf(inv, 2, "%s", sshUsage)
	}
	if _, err := parseSSHTarget(inv.Args[0]); err != nil {
		return commands.Exitf(inv, 2, "ssh: %v", err)
	}
	opts, err := c.sshOptions(ctx, inv.Args[0], 0)
	if err != nil {
		return commands.Exitf(inv, 126, "ssh: %v", err)
	}
	opts.Exec = append(opts.Exec, strings.Join(inv.Args[1:], " "))
	code, err := c.runSSH(ctx, opts, inv.Stdin, inv.Stdout, inv.Stderr)
	if err != nil {
		return commands.Exitf(inv, 126, "ssh: %v", err)
	}
	if code != 0 {
		return &commands.ExitError{Code: code}
	}
	return nil
}

// withSFTP owns every pipe and the process. Context cancellation also closes
// protocol pipes: killing ssh alone cannot unblock every io.Pipe operation.
func (c *Client) withSFTP(ctx context.Context, opts vbox.StartOptions, stderr io.Writer, fn func(*sftp.Client) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Insert -s before --; no user arguments are appended as SSH options.
	i := len(opts.Exec) - 2
	opts.Exec = append(append(append([]string{}, opts.Exec[:i]...), "-s"), opts.Exec[i:]...)
	opts.Exec = append(opts.Exec, "sftp")
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	closePipes := func(err error) {
		inR.CloseWithError(err)
		inW.CloseWithError(err)
		outR.CloseWithError(err)
		outW.CloseWithError(err)
	}
	defer closePipes(io.EOF)
	stop := context.AfterFunc(ctx, func() { closePipes(ctx.Err()) })
	defer stop()
	done := make(chan error, 1)
	go func() {
		code, err := c.runSSH(ctx, opts, inR, outW, stderr)
		if err == nil && code != 0 {
			err = fmt.Errorf("SSH transport exited %d", code)
		}
		outW.CloseWithError(err)
		inR.CloseWithError(err)
		done <- err
	}()
	client, err := sftp.NewClientPipe(outR, inW)
	if err == nil {
		err = fn(client)
	}
	// Client.Close waits for the receive loop. Send EOF first and bound the
	// process wait before closing the client, even for a non-cooperating server.
	inW.Close()
	if err != nil {
		cancel()
	}
	// Bound graceful shutdown even if a server keeps its subsystem alive.
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case processErr := <-done:
		if err == nil {
			err = processErr
		}
	case <-timer.C:
		cancel()
		processErr := <-done
		if err == nil {
			err = fmt.Errorf("SSH subsystem did not exit: %v", processErr)
		}
	}
	outR.Close()
	if client != nil {
		_ = client.Close()
	}
	return err
}

// errStdoutClosed 表示批处理的输出下游已关闭（`sftp -b - ... | head`）：这不是
// 某一行失败，而是 SIGPIPE 语义的正常终止——不打印 stderr、不带批处理行号，
// 由调用方转成 ExitError{141}。
var errStdoutClosed = errors.New("sftp: output closed by downstream")

func transferError(inv *commands.Invocation, name string, err error) error {
	if err == nil {
		return nil
	}
	code := 1
	if errors.Is(err, fs.ErrPermission) || errors.Is(err, execution.ErrRuleDenied) {
		code = 126
	}
	return commands.Exitf(inv, code, "%s: %v", name, err)
}
