package host

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"github.com/veypi/aic-pod/protocol"
	"io"
	"io/fs"
	"os"
	"path"
	"runtime"
	"strings"

	"github.com/pkg/sftp"
	"github.com/veypi/aic-pod/libs/execution"

	"github.com/veypi/vbox"
	"github.com/veypi/vsh/commands"
)

const scpUsage = "usage: scp [-r] [-q] [-P port] <source> <destination>\nExactly one operand must be [user@]host:path. SFTP transport; literal paths, no glob/symlink/special files. Local relative paths use the invocation cwd. Windows remote absolute paths: C:/path or /C:/path (forward slashes)."
const sftpUsage = "usage: sftp [-q] [-P port] -b <file|-> <target>\nBatch commands: get/put [-r] source destination, ls [path], pwd, cd path, stat path, mkdir path, rmdir path, rm path, rename old new, quit. Literal paths; no local commands, glob, resume, or error-suppression prefixes. Maximum batch size 1 MiB. Windows remote absolute paths: C:/path or /C:/path (forward slashes)."

type sshTransferArgs struct {
	port      int
	recursive bool
	batch     string
	args      []string
}

func parseSSHTransferArgs(args []string, batch bool) (sshTransferArgs, error) {
	var o sshTransferArgs
	for len(args) > 0 {
		a := args[0]
		if a == "--" {
			args = args[1:]
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			break
		}
		args = args[1:]
		switch a {
		case "-q":
		case "-r":
			if batch {
				return o, fmt.Errorf("-r belongs in get/put batch commands")
			}
			o.recursive = true
		case "-P", "-b":
			if len(args) == 0 {
				return o, fmt.Errorf("%s requires a value", a)
			}
			if a == "-P" {
				p, err := sshPort(args[0])
				if err != nil {
					return o, err
				}
				if o.port != 0 {
					return o, fmt.Errorf("duplicate -P")
				}
				o.port = p
			} else {
				if !batch || o.batch != "" || args[0] == "" {
					return o, fmt.Errorf("invalid -b")
				}
				o.batch = args[0]
			}
			args = args[1:]
		default:
			return o, fmt.Errorf("unsupported option %s", a)
		}
	}
	o.args = args
	if batch && (o.batch == "" || len(args) != 1) || !batch && len(args) != 2 {
		return o, fmt.Errorf("invalid operands")
	}
	return o, nil
}

type sshOperand struct {
	target, name string
	remote       bool
}

func parseSSHOperand(s string) (sshOperand, error) {
	o := sshOperand{name: sshLocalPath(s)}
	if s == "" || strings.ContainsAny(s, "\x00\r\n") {
		return o, fmt.Errorf("invalid path")
	}
	if len(s) >= 3 && s[1] == ':' && strings.ContainsRune("/\\", rune(s[2])) {
		return o, nil
	}
	bracket := false
	for i, ch := range s {
		if ch == '/' || ch == '\\' {
			return o, nil
		}
		if ch == '[' {
			bracket = true
		}
		if ch == ']' {
			bracket = false
		}
		if ch == ':' && !bracket {
			o = sshOperand{target: s[:i], name: s[i+1:], remote: true}
			if _, err := parseSSHTarget(o.target); err != nil {
				return o, err
			}
			if err := literalRemotePath(o.name); err != nil {
				return o, err
			}
			return o, nil
		}
	}
	return o, nil
}

func sshLocalPath(s string) string {
	if runtime.GOOS == "windows" {
		return protocol.NormalizeHostPath(s)
	}
	return s
}

func (c *Client) preflightSSHLocal(ctx context.Context, inv *commands.Invocation, name string, write bool) error {
	op := vbox.OpRead
	if write {
		op = vbox.OpWrite
	}
	abs := inv.FS.Resolve(name)
	if !c.perms.fsSnapshot(execution.SessionFromContext(ctx)).Match(abs, op).Allow {
		return fmt.Errorf("%w: request grant fs %s", execution.ErrRuleDenied, abs)
	}
	return nil
}

func literalRemotePath(s string) error {
	if s == "" || strings.ContainsAny(s, "\x00\r\n*?[]\\") {
		return fmt.Errorf("expected a literal remote path without glob or control characters")
	}
	if strings.HasPrefix(s, "~") {
		return fmt.Errorf("remote ~ expansion is unsupported; use an absolute or server-relative path")
	}
	if sshDrivePath(s) && (len(s) == 2 || s[2] != '/') {
		return fmt.Errorf("remote drive-relative paths are unsupported; use C:/path or /C:/path")
	}
	return nil
}

func sshDrivePath(s string) bool {
	return len(s) >= 2 && s[1] == ':' && (s[0] >= 'a' && s[0] <= 'z' || s[0] >= 'A' && s[0] <= 'Z')
}

// SFTP paths use slash separators independently of the client OS. In
// particular, a Windows drive path must not be joined to the remote cwd.
func resolveSSHRemotePath(cwd, name string) string {
	canonical := func(p string) string {
		if sshDrivePath(p) && len(p) >= 3 && p[2] == '/' {
			return "/" + p
		}
		return p
	}
	clean := func(p string) string {
		// Keep .. at a drive root on that drive, like Windows path resolution.
		if len(p) >= 3 && p[0] == '/' && sshDrivePath(p[1:]) && (len(p) == 3 || p[3] == '/') {
			return p[:3] + path.Clean("/"+p[3:])
		}
		return path.Clean(p)
	}
	name = canonical(name)
	if path.IsAbs(name) {
		return clean(name)
	}
	if cwd == "" {
		return clean(name)
	}
	return clean(canonical(cwd) + "/" + name)
}

func safeTransferName(s string) bool {
	if s == "" || s == "." || s == ".." || strings.ContainsAny(s, "/\\:\x00\r\n") || strings.TrimRight(s, " .") != s {
		return false
	}
	// Reject Windows device names on every host so a downloaded tree is portable.
	n := strings.ToUpper(strings.SplitN(s, ".", 2)[0])
	if n == "CON" || n == "PRN" || n == "AUX" || n == "NUL" || n == "CONIN$" || n == "CONOUT$" {
		return false
	}
	if len(n) == 4 && (strings.HasPrefix(n, "COM") || strings.HasPrefix(n, "LPT")) && n[3] >= '1' && n[3] <= '9' {
		return false
	}
	return true
}

func (c *Client) managedSCP(ctx context.Context, inv *commands.Invocation) error {
	if sshHelp(inv, scpUsage) {
		return nil
	}
	o, err := parseSSHTransferArgs(inv.Args, false)
	if err != nil {
		return commands.Exitf(inv, 2, "scp: %v\n%s", err, scpUsage)
	}
	src, err := parseSSHOperand(o.args[0])
	if err != nil {
		return commands.Exitf(inv, 2, "scp: %v", err)
	}
	dst, err := parseSSHOperand(o.args[1])
	if err != nil {
		return commands.Exitf(inv, 2, "scp: %v", err)
	}
	if src.remote == dst.remote {
		return commands.Exitf(inv, 2, "scp: exactly one remote operand required (use cp for local copies)")
	}
	target := dst.target
	if src.remote {
		target = src.target
	}
	opts, err := c.sshOptions(ctx, target, o.port)
	if err != nil {
		return commands.Exitf(inv, 126, "scp: %v", err)
	}
	local := src.name
	if src.remote {
		local = dst.name
	}
	if err := c.preflightSSHLocal(ctx, inv, local, src.remote); err != nil {
		return transferError(inv, "scp", err)
	}
	if !src.remote {
		if _, err := localTransferInfo(ctx, inv, src.name); err != nil {
			return transferError(inv, "scp", err)
		}
	}
	err = c.withSFTP(ctx, opts, inv.Stderr, func(remote *sftp.Client) error {
		cwd, err := remote.Getwd()
		if err != nil {
			return err
		}
		if src.remote {
			src.name = resolveSSHRemotePath(cwd, src.name)
		} else {
			dst.name = resolveSSHRemotePath(cwd, dst.name)
		}
		t := sshTransfer{ctx: ctx, inv: inv, remote: remote}
		return t.copy(src.name, dst.name, !src.remote, o.recursive, 0)
	})
	return transferError(inv, "scp", err)
}

type sftpOperation struct {
	command   string
	args      []string
	recursive bool
	line      int
}

func parseSFTPBatch(r io.Reader) ([]sftpOperation, error) {
	data, err := io.ReadAll(io.LimitReader(r, sshConfigLimit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > sshConfigLimit {
		return nil, fmt.Errorf("batch exceeds 1 MiB")
	}
	var ops []sftpOperation
	s := bufio.NewScanner(strings.NewReader(string(data)))
	s.Buffer(make([]byte, 4096), sshConfigLimit+1)
	line := 0
	for s.Scan() {
		line++
		w, err := sshWords(s.Text())
		if err != nil {
			return nil, fmt.Errorf("batch line %d: %w", line, err)
		}
		if len(w) == 0 {
			continue
		}
		op := sftpOperation{command: w[0], args: w[1:], line: line}
		min, max := 0, 0
		switch op.command {
		case "get", "put":
			min, max = 2, 2
			if len(op.args) > 0 && op.args[0] == "-r" {
				op.recursive = true
				op.args = op.args[1:]
			}
		case "rename":
			min, max = 2, 2
		case "ls":
			min, max = 0, 1
		case "pwd", "quit":
		case "cd", "stat", "mkdir", "rmdir", "rm":
			min, max = 1, 1
		default:
			return nil, fmt.Errorf("batch line %d: unsupported command %q", line, op.command)
		}
		if len(op.args) < min || len(op.args) > max {
			return nil, fmt.Errorf("batch line %d: invalid %s operands", line, op.command)
		}
		for i, p := range op.args {
			local := op.command == "get" && i == 1 || op.command == "put" && i == 0
			if local {
				if p == "" || strings.ContainsAny(p, "\x00\r\n*?[]") {
					return nil, fmt.Errorf("batch line %d: invalid local path", line)
				}
				op.args[i] = sshLocalPath(p)
			} else if err := literalRemotePath(p); err != nil {
				return nil, fmt.Errorf("batch line %d: %w", line, err)
			}
		}
		ops = append(ops, op)
		if len(ops) > 4096 {
			return nil, fmt.Errorf("too many batch operations")
		}
	}
	return ops, s.Err()
}

func (c *Client) managedSFTP(ctx context.Context, inv *commands.Invocation) error {
	if sshHelp(inv, sftpUsage) {
		return nil
	}
	o, err := parseSSHTransferArgs(inv.Args, true)
	if err != nil {
		return commands.Exitf(inv, 2, "sftp: %v\n%s", err, sftpUsage)
	}
	r := inv.Stdin
	if o.batch != "-" {
		f, err := inv.FS.Open(ctx, o.batch)
		if err != nil {
			return transferError(inv, "sftp", err)
		}
		defer f.Close()
		r = f
	}
	if r == nil {
		r = strings.NewReader("")
	}
	ops, err := parseSFTPBatch(r)
	if err != nil {
		return commands.Exitf(inv, 2, "sftp: %v", err)
	}
	opts, err := c.sshOptions(ctx, o.args[0], o.port)
	if err != nil {
		return commands.Exitf(inv, 126, "sftp: %v", err)
	}
	for _, op := range ops {
		if op.command == "get" {
			err = c.preflightSSHLocal(ctx, inv, op.args[1], true)
		}
		if op.command == "put" {
			err = c.preflightSSHLocal(ctx, inv, op.args[0], false)
		}
		if err != nil {
			return transferError(inv, "sftp", err)
		}
	}
	err = c.withSFTP(ctx, opts, inv.Stderr, func(remote *sftp.Client) error {
		cwd, err := remote.Getwd()
		if err != nil {
			return err
		}
		cwd = resolveSSHRemotePath("", cwd)
		t := sshTransfer{ctx: ctx, inv: inv, remote: remote}
		resolve := func(p string) string {
			return resolveSSHRemotePath(cwd, p)
		}
		for _, op := range ops {
			if err := ctx.Err(); err != nil {
				return err
			}
			var err error
			a := op.args
			switch op.command {
			case "quit":
				return nil
			case "pwd":
				_, err = fmt.Fprintln(inv.Stdout, cwd)
				if commands.BrokenPipe(err) {
					return errStdoutClosed
				}
			case "get":
				err = t.copy(resolve(a[0]), a[1], false, op.recursive, 0)
			case "put":
				err = t.copy(a[0], resolve(a[1]), true, op.recursive, 0)
			case "cd":
				var fi fs.FileInfo
				next := resolve(a[0])
				fi, err = remote.Stat(next)
				if err == nil {
					if !fi.IsDir() {
						err = fmt.Errorf("not a directory")
					} else {
						cwd = next
					}
				}
			case "ls":
				p := cwd
				if len(a) > 0 {
					p = resolve(a[0])
				}
				var entries []fs.FileInfo
				entries, err = remote.ReadDirContext(ctx, p)
				if err == nil {
					for _, e := range entries {
						if _, err = fmt.Fprintln(inv.Stdout, e.Name()); err != nil {
							break
						}
					}
					if commands.BrokenPipe(err) {
						return errStdoutClosed
					}
				}
			case "stat":
				var fi fs.FileInfo
				fi, err = remote.Lstat(resolve(a[0]))
				if err == nil {
					_, err = fmt.Fprintf(inv.Stdout, "%s %d %s\n", fi.Mode(), fi.Size(), a[0])
					if commands.BrokenPipe(err) {
						return errStdoutClosed
					}
				}
			case "mkdir":
				err = remote.Mkdir(resolve(a[0]))
			case "rmdir":
				err = remote.RemoveDirectory(resolve(a[0]))
			case "rm":
				err = remote.Remove(resolve(a[0]))
			case "rename":
				err = remote.Rename(resolve(a[0]), resolve(a[1]))
			}
			if err != nil {
				return fmt.Errorf("batch line %d (%s): %w", op.line, op.command, err)
			}
		}
		return nil
	})
	if errors.Is(err, errStdoutClosed) {
		return &commands.ExitError{Code: 141}
	}
	return transferError(inv, "sftp", err)
}

type sshTransfer struct {
	ctx     context.Context
	inv     *commands.Invocation
	remote  *sftp.Client
	entries int
}

func localTransferInfo(ctx context.Context, inv *commands.Invocation, name string) (fs.FileInfo, error) {
	fi, err := inv.FS.Lstat(ctx, name)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() && !fi.IsDir() {
		return nil, fmt.Errorf("local symlink/special file unsupported: %s", name)
	}
	return fi, nil
}

func (t *sshTransfer) copy(src, dst string, upload, recursive bool, depth int) error {
	if err := t.ctx.Err(); err != nil {
		return err
	}
	t.entries++
	if t.entries > 100000 || depth > 64 {
		return fmt.Errorf("transfer tree limit exceeded")
	}
	var info fs.FileInfo
	var err error
	if upload {
		info, err = localTransferInfo(t.ctx, t.inv, src)
	} else {
		info, err = t.remote.Lstat(src)
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() && !info.IsDir() {
		return fmt.Errorf("symlink/special file unsupported: %s", src)
	}
	if depth == 0 {
		var target fs.FileInfo
		if upload {
			target, err = t.remote.Lstat(dst)
		} else {
			target, err = t.inv.FS.Lstat(t.ctx, dst)
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err == nil && target.IsDir() {
			base := path.Base(src)
			if upload {
				base = path.Base(t.inv.FS.Resolve(src))
			}
			if !safeTransferName(base) {
				return fmt.Errorf("unsafe source basename %q", base)
			}
			dst = path.Join(dst, base)
		} else if err == nil && !target.Mode().IsRegular() {
			return fmt.Errorf("destination is a symlink/special file")
		}
	}
	if info.IsDir() {
		if !recursive {
			return fmt.Errorf("%s is a directory; use -r", src)
		}
		if upload {
			if err := t.remote.Mkdir(dst); err != nil {
				fi, e := t.remote.Lstat(dst)
				if e != nil || !fi.IsDir() {
					return err
				}
			}
			entries, err := t.inv.FS.ReadDir(t.ctx, src)
			if err != nil {
				return err
			}
			for _, e := range entries {
				if !safeTransferName(e.Name()) {
					return fmt.Errorf("unsafe entry %q", e.Name())
				}
				if err := t.copy(path.Join(src, e.Name()), path.Join(dst, e.Name()), true, true, depth+1); err != nil {
					return err
				}
			}
		} else {
			if fi, err := t.inv.FS.Lstat(t.ctx, dst); err == nil && !fi.IsDir() {
				return fmt.Errorf("destination is not a directory")
			} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			if err := t.inv.FS.MkdirAll(t.ctx, dst, 0700); err != nil {
				return err
			}
			entries, err := t.remote.ReadDirContext(t.ctx, src)
			if err != nil {
				return err
			}
			for _, e := range entries {
				if !safeTransferName(e.Name()) {
					return fmt.Errorf("unsafe remote entry %q", e.Name())
				}
				if err := t.copy(path.Join(src, e.Name()), path.Join(dst, e.Name()), false, true, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if max := t.inv.Limits.MaxFileBytes; max > 0 && info.Size() > max {
		return fmt.Errorf("file exceeds session size limit")
	}
	if upload {
		return t.upload(src, dst, info)
	}
	return t.download(src, dst)
}

type sshContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r sshContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func (t *sshTransfer) stream(dst io.Writer, src io.Reader) error {
	r := io.Reader(sshContextReader{t.ctx, src})
	if max := t.inv.Limits.MaxFileBytes; max > 0 {
		n, err := io.Copy(dst, io.LimitReader(r, max))
		if err != nil {
			return err
		}
		if n == max {
			var b [1]byte
			n, err := r.Read(b[:])
			if n != 0 {
				return fmt.Errorf("file exceeds session size limit")
			}
			if err != nil && err != io.EOF {
				return err
			}
		}
		return nil
	}
	_, err := io.Copy(dst, r)
	return err
}

func (t *sshTransfer) upload(src, dst string, info fs.FileInfo) error {
	in, err := t.inv.FS.Open(t.ctx, src)
	if err != nil {
		return err
	}
	defer in.Close()
	actual, err := in.Stat()
	if err != nil {
		return err
	}
	if !actual.Mode().IsRegular() || !os.SameFile(info, actual) {
		return fmt.Errorf("local source changed before open")
	}
	if fi, err := t.remote.Lstat(dst); err == nil && !fi.Mode().IsRegular() {
		return fmt.Errorf("remote destination is a symlink/special file")
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	out, err := t.remote.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return err
	}
	err = t.stream(out, in)
	closeErr := out.Close()
	if err == nil {
		err = closeErr
	}
	return err
}

func (t *sshTransfer) download(src, dst string) error {
	dst = t.inv.FS.Resolve(dst)
	if fi, err := t.inv.FS.Lstat(t.ctx, dst); err == nil && !fi.Mode().IsRegular() {
		return fmt.Errorf("local destination is a symlink/special file")
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	// Use exclusive creation; a pre-existing .part file can never be truncated.
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	temp := path.Join(path.Dir(dst), fmt.Sprintf(".aic-sftp-%x.part", random))
	out, err := t.inv.FS.OpenFile(t.ctx, temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() {
		_ = out.Close() // close before unlink, including failures opening the remote source
		if err := t.inv.FS.Remove(context.WithoutCancel(t.ctx), temp, false); err != nil && !errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(t.inv.Stderr, "sftp: partial file cleanup failed for %s: %v\n", temp, err)
		}
	}()
	in, err := t.remote.Open(src)
	if err != nil {
		return err
	}
	err = t.stream(out, in)
	closeErr := in.Close()
	if err == nil {
		err = closeErr
	}
	if syncer, ok := out.(interface{ Sync() error }); err == nil && ok {
		err = syncer.Sync()
	}
	closeErr = out.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := t.ctx.Err(); err != nil {
		return err
	}
	return t.inv.FS.Rename(t.ctx, temp, dst)
}
