package host

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/execution"
	"github.com/veypi/vbox"
	"github.com/veypi/vsh/commands"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// A real, loopback-only SSH server exercises the installed native client. It
// only implements fixed test commands and an SFTP subsystem, never a shell.
func startManagedSSHFixture(t *testing.T, home, remoteDir string) (string, *atomic.Int32) {
	t.Helper()
	if _, err := trustedSSHBinary(); err != nil {
		t.Skip(err)
	}
	_, hostKey, _ := ed25519.GenerateKey(rand.Reader)
	hostSigner, _ := ssh.NewSignerFromKey(hostKey)
	pub, private, _ := ed25519.GenerateKey(rand.Reader)
	authorized, _ := ssh.NewPublicKey(pub)
	serverConfig := &ssh.ServerConfig{PublicKeyCallback: func(c ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if c.User() != "fixture" || string(key.Marshal()) != string(authorized.Marshal()) {
			return nil, fmt.Errorf("unauthorized")
		}
		return &ssh.Permissions{}, nil
	}}
	serverConfig.AddHostKey(hostSigner)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var connections sync.Map
	var wg sync.WaitGroup
	calls := &atomic.Int32{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			connections.Store(conn, true)
			calls.Add(1)
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer conn.Close()
				defer connections.Delete(conn)
				ss, channels, requests, err := ssh.NewServerConn(conn, serverConfig)
				if err != nil {
					return
				}
				defer ss.Close()
				go ssh.DiscardRequests(requests)
				for incoming := range channels {
					if incoming.ChannelType() != "session" {
						incoming.Reject(ssh.UnknownChannelType, "session only")
						continue
					}
					ch, reqs, err := incoming.Accept()
					if err != nil {
						continue
					}
					wg.Add(1)
					go func() {
						defer wg.Done()
						defer ch.Close()
						for req := range reqs {
							var value struct{ Value string }
							_ = ssh.Unmarshal(req.Payload, &value)
							switch req.Type {
							case "exec":
								req.Reply(true, nil)
								status := uint32(0)
								switch value.Value {
								case "binary":
									_, _ = ch.Write([]byte{0, 0xff, 0x81, 'x', '\n', 0})
								case "exit-seven":
									status = 7
								case "wait":
									_, _ = io.Copy(io.Discard, ch)
								default:
									status = 2
								}
								ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
								return
							case "subsystem":
								if value.Value != "sftp" {
									req.Reply(false, nil)
									continue
								}
								req.Reply(true, nil)
								server, err := sftp.NewServer(ch, sftp.WithServerWorkingDirectory(remoteDir))
								if err == nil {
									_ = server.Serve()
									_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
									_ = server.Close()
								}
								return
							default:
								req.Reply(false, nil)
							}
						}
					}()
				}
			}()
		}
	}()
	t.Cleanup(func() {
		l.Close()
		connections.Range(func(k, v any) bool { k.(net.Conn).Close(); return true })
		wg.Wait()
	})
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, "test-only")
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(sshDir, "fixture_key")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatal(err)
	}
	address := l.Addr().String()
	if err := os.WriteFile(filepath.Join(sshDir, "known_hosts"), []byte(knownhosts.Line([]string{address}, hostSigner.PublicKey())+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(address)
	writeSSHTestConfig(t, home, "Host fixture\n HostName 127.0.0.1\n User fixture\n Port "+port+"\n IdentityFile \""+filepath.ToSlash(keyFile)+"\"\n IdentitiesOnly yes\n")
	return address, calls
}

func newManagedSSHTestClient(t *testing.T) *Client {
	t.Helper()
	saved := cfg.Global
	cfg.Global = cfg.NewOptions()
	t.Cleanup(func() { cfg.Global = saved })
	c, _ := testClient(t)
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", os.Getenv("HOME"))
	}
	return c
}

func runManagedScript(t *testing.T, c *Client, script, stdin string) *execution.ExecResult {
	t.Helper()
	e, err := c.engine()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r, err := e.Exec(ctx, execution.ExecRequest{SessionKey: "ssh-test", Script: script, WorkDir: hostCanonical(c.options().WorkDir), Env: map[string]string{"HOME": "/untrusted", "PATH": "/untrusted", "SSH_AUTH_SOCK": "/untrusted", "SSH_ASKPASS": "/untrusted"}, Stdin: strings.NewReader(stdin), GrantApproved: true})
	if err != nil {
		t.Fatalf("%s: %v", script, err)
	}
	return r
}

func TestManagedSSHNativeIntegration(t *testing.T) {
	c := newManagedSSHTestClient(t)
	home, _ := os.UserHomeDir()
	remote := t.TempDir()
	address, calls := startManagedSSHFixture(t, home, remote)
	if r := runManagedScript(t, c, "ssh fixture binary", ""); r.ExitCode != 126 || calls.Load() != 0 || !strings.Contains(r.Stderr, "grant ssh "+address) {
		t.Fatalf("denial %+v connections=%d", r, calls.Load())
	}
	payload := string([]byte{0, 0xff, 0x81, 'x', '\n', 0})
	if r := runManagedScript(t, c, "grant ssh "+address+"; ssh fixture binary", ""); r.ExitCode != 0 || !strings.HasSuffix(r.Stdout, payload) {
		t.Fatalf("grant/exec %+v", r)
	}
	for _, script := range []string{"ssh fixture binary", "env ssh fixture binary", "timeout 5 ssh fixture binary", "bash -c 'ssh fixture binary'", "printf 'binary\\n' | xargs ssh fixture"} {
		if r := runManagedScript(t, c, script, ""); r.ExitCode != 0 || r.Stdout != payload {
			t.Fatalf("%s: %+v", script, r)
		}
	}
	if r := runManagedScript(t, c, "ssh fixture exit-seven", ""); r.ExitCode != 7 {
		t.Fatalf("exit %+v", r)
	}
	for _, script := range []string{"ssh fixture", "ssh -o x fixture binary", "grant cmd ssh"} {
		if r := runManagedScript(t, c, script, ""); r.ExitCode == 0 {
			t.Fatalf("accepted %s", script)
		}
	}
	if runtime.GOOS != "windows" {
		if r := runManagedScript(t, c, "/usr/bin/ssh fixture binary", ""); r.ExitCode != 126 {
			t.Fatalf("native bypass %+v", r)
		}
	}
	// Unknown host keys never enroll themselves.
	if err := os.WriteFile(filepath.Join(home, ".ssh", "known_hosts"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if r := runManagedScript(t, c, "ssh fixture binary", ""); r.ExitCode != 255 {
		t.Fatalf("unknown host key %+v", r)
	}
	data, _ := os.ReadFile(filepath.Join(home, ".ssh", "known_hosts"))
	if len(data) != 0 {
		t.Fatal("host key automatically enrolled")
	}
}

func TestManagedSFTPTransfersAndRules(t *testing.T) {
	c := newManagedSSHTestClient(t)
	home, _ := os.UserHomeDir()
	remote := t.TempDir()
	address, _ := startManagedSSHFixture(t, home, remote)
	if r := runManagedScript(t, c, "grant ssh "+address, ""); r.ExitCode != 0 {
		t.Fatal(r)
	}
	work := c.options().WorkDir
	payload := strings.Repeat("binary\x00\xff", 10000)
	if err := os.WriteFile(filepath.Join(work, "source file"), []byte(payload), 0600); err != nil {
		t.Fatal(err)
	}
	for _, script := range []string{"scp 'source file' fixture:uploaded", "scp fixture:uploaded downloaded", "sftp -b - fixture"} {
		batch := "put 'source file' batch-upload\nget batch-upload batch-download\nstat batch-upload\npwd\n"
		r := runManagedScript(t, c, script, batch)
		if r.ExitCode != 0 {
			t.Fatalf("%s: %+v", script, r)
		}
		if script == "scp fixture:uploaded downloaded" {
			found := false
			for _, p := range r.Writes {
				if strings.HasSuffix(p, "/downloaded") {
					found = true
				}
			}
			if !found {
				t.Fatalf("download missing from fs write audit: %+v", r)
			}
		}
	}
	for _, file := range []string{filepath.Join(remote, "uploaded"), filepath.Join(work, "downloaded"), filepath.Join(work, "batch-download")} {
		data, err := os.ReadFile(file)
		if err != nil || string(data) != payload {
			t.Fatalf("binary mismatch %s %v", file, err)
		}
	}
	// 输出下游提前关闭（`| head`）：与内建命令同一约定，按 SIGPIPE 收尾（141）——
	// 不是批处理某一行失败，不打印 stderr；管道整体状态仍取末命令，脚本继续。
	closed := strings.Repeat("pwd\n", 500)
	if r := runManagedScript(t, c, "sftp -b - fixture | head -1; echo after:$? ps:${PIPESTATUS[@]}", closed); r.ExitCode != 0 || r.Stderr != "" || !strings.HasSuffix(r.Stdout, "after:0 ps:141 0\n") {
		t.Fatalf("closed pipe %+v", r)
	}
	// Failure must leave an existing destination intact and remove .part files.
	if r := runManagedScript(t, c, "scp fixture:missing downloaded", ""); r.ExitCode == 0 {
		t.Fatal("missing source accepted")
	}
	data, _ := os.ReadFile(filepath.Join(work, "downloaded"))
	if string(data) != payload {
		t.Fatal("failed download destroyed target")
	}
	parts, _ := filepath.Glob(filepath.Join(work, ".aic-sftp-*.part"))
	if len(parts) != 0 {
		t.Fatalf("partial leak %v", parts)
	}
	// A denied descendant cannot be bypassed by checking only its parent.
	dir := filepath.Join(work, "tree")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "data"), []byte(payload), 0600); err != nil {
		t.Fatal(err)
	}
	for _, script := range []string{"scp -r tree fixture:recursive", "scp -r fixture:recursive tree-copy"} {
		if r := runManagedScript(t, c, script, ""); r.ExitCode != 0 {
			t.Fatalf("recursive transfer: %+v", r)
		}
	}
	data, err := os.ReadFile(filepath.Join(work, "tree-copy", "sub", "data"))
	if err != nil || string(data) != payload {
		t.Fatalf("recursive mismatch %v", err)
	}
	os.WriteFile(filepath.Join(dir, "secret"), []byte("secret"), 0600)
	cfg.Global.FsRules = []string{"deny:" + hostCanonical(filepath.Join(dir, "secret")), "deny:" + hostCanonical(filepath.Join(work, "denied"))}
	publishGlobal(t, c.perms)
	if r := runManagedScript(t, c, "scp -r tree fixture:tree", ""); r.ExitCode != 126 {
		t.Fatalf("descendant deny %+v", r)
	}
	if _, err := os.Stat(filepath.Join(remote, "tree", "secret")); !os.IsNotExist(err) {
		t.Fatal("denied descendant uploaded")
	}
	if r := runManagedScript(t, c, "scp fixture:uploaded denied", ""); r.ExitCode != 126 {
		t.Fatalf("download deny %+v", r)
	}
	parts, _ = filepath.Glob(filepath.Join(work, ".aic-sftp-*.part"))
	if len(parts) != 0 {
		t.Fatalf("partial leak %v", parts)
	}
	if err := os.Symlink(filepath.Join(work, "source file"), filepath.Join(work, "link")); err == nil {
		if r := runManagedScript(t, c, "scp link fixture:link", ""); r.ExitCode == 0 {
			t.Fatal("symlink uploaded")
		}
	}
}

func TestManagedSSHSessionIsolationAndStalledTransport(t *testing.T) {
	c := newManagedSSHTestClient(t)
	calls := 0
	c.sshRun = func(ctx context.Context, _ vbox.StartOptions, _ io.Reader, _ io.Writer, _ io.Writer) (int, error) {
		calls++
		<-ctx.Done()
		return 0, ctx.Err()
	}
	home, _ := os.UserHomeDir()
	writeSSHTestConfig(t, home, "Host test\n HostName 192.0.2.1\n User test\n")
	e, _ := c.engine()
	r, err := e.Exec(context.Background(), execution.ExecRequest{SessionKey: "one", Script: "grant ssh 192.0.2.1:22", GrantApproved: true})
	if err != nil || r.ExitCode != 0 {
		t.Fatal(r, err)
	}
	r, err = e.Exec(context.Background(), execution.ExecRequest{SessionKey: "two", Script: "ssh test binary"})
	if err != nil || r.ExitCode != 126 || calls != 0 {
		t.Fatalf("session leak %+v %v", r, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _ = e.Exec(ctx, execution.ExecRequest{SessionKey: "one", Script: "sftp -b - test", Stdin: strings.NewReader("pwd\n")})
	if calls != 1 || time.Since(start) > 2*time.Second {
		t.Fatal("stalled protocol did not cancel")
	}
}

func TestManagedSSHNativeNames(t *testing.T) {
	c := &Client{}
	for _, name := range []string{"ssh", "scp", "sftp", "slogin", "ssh-copy-id", "ssh-keyscan"} {
		var stderr strings.Builder
		err := c.nativeExec(context.Background(), "/bin/"+name, &commands.Invocation{Stderr: &stderr})
		if code, _ := commands.ExitCode(err); code != 126 {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

type failedSFTPRead struct{ bytes atomic.Int64 }

func (f *failedSFTPRead) Fileread(*sftp.Request) (io.ReaderAt, error) { return f, nil }
func (f *failedSFTPRead) ReadAt(p []byte, off int64) (int, error) {
	if off >= 32768 {
		return 0, errors.New("injected remote read failure")
	}
	n := len(p)
	if n > 32768-int(off) {
		n = 32768 - int(off)
	}
	for i := 0; i < n; i++ {
		p[i] = 0xab
	}
	f.bytes.Add(int64(n))
	return n, nil
}

type sftpFixturePipe struct {
	io.Reader
	io.Writer
}

func (sftpFixturePipe) Close() error { return nil }

type windowsSFTPFixtureLister struct {
	sftp.FileLister
	cwd string
}

func (f windowsSFTPFixtureLister) RealPath(p string) (string, error) {
	if p != "." {
		return "", fmt.Errorf("fixture only resolves the initial cwd")
	}
	return f.cwd, nil
}

func TestManagedSFTPWindowsRemotePaths(t *testing.T) {
	// Test the remote path grammar on every client OS, using a protocol server
	// whose filesystem contains Windows drive roots rather than host OS paths.
	for _, cwd := range []string{"/C:/Users/v", "C:/Users/v"} {
		t.Run(cwd, func(t *testing.T) {
			c := newManagedSSHTestClient(t)
			handlers := sftp.InMemHandler()
			for _, dir := range []string{"/C:", "/C:/Users", "/C:/Users/v", "/D:", "/D:/work"} {
				if err := handlers.FileCmd.Filecmd(&sftp.Request{Method: "Mkdir", Filepath: dir}); err != nil {
					t.Fatal(err)
				}
			}
			handlers.FileList = windowsSFTPFixtureLister{handlers.FileList, cwd}
			c.sshRun = func(ctx context.Context, _ vbox.StartOptions, in io.Reader, out, _ io.Writer) (int, error) {
				server := sftp.NewRequestServer(sftpFixturePipe{in, out}, handlers, sftp.WithStartDirectory("/C:/Users/v"))
				defer server.Close()
				err := server.Serve()
				if errors.Is(err, io.EOF) {
					err = nil
				}
				return 0, err
			}
			work := c.options().WorkDir
			payload := []byte("binary\x00\xff\x81\n")
			if err := os.WriteFile(filepath.Join(work, "source"), payload, 0600); err != nil {
				t.Fatal(err)
			}
			for _, script := range []string{
				"grant ssh fixture:22",
				"scp source fixture:C:/Users/v/from-scp",
				"scp fixture:/C:/Users/v/from-scp scp-download",
			} {
				if r := runManagedScript(t, c, script, ""); r.ExitCode != 0 {
					t.Fatalf("%s: %+v", script, r)
				}
			}
			batch := "mkdir C:/Users/v/dir\n" +
				"put source C:/Users/v/dir/upload\n" +
				"stat /C:/Users/v/dir/upload\n" +
				"rename C:/Users/v/dir/upload /C:/Users/v/dir/renamed\n" +
				"cd C:/Users/v/dir\npwd\nls\nget renamed batch-download\n" +
				"mkdir nested\nput source nested/file\nget -r C:/Users/v/dir/nested tree\n" +
				"rm nested/file\nrmdir C:/Users/v/dir/nested\nrm renamed\n" +
				"cd D:/work\nput source other-drive\nget /D:/work/other-drive drive-download\n" +
				"rm other-drive\nrmdir /C:/Users/v/dir\nrm C:/Users/v/from-scp\nquit\n"
			if r := runManagedScript(t, c, "sftp -b - fixture", batch); r.ExitCode != 0 || !strings.Contains(r.Stdout, "/C:/Users/v/dir\n") {
				t.Fatalf("Windows batch: %+v", r)
			}
			for _, script := range []string{
				"scp -r tree fixture:D:/work/tree",
				"scp -r fixture:/D:/work/tree scp-tree",
			} {
				if r := runManagedScript(t, c, script, ""); r.ExitCode != 0 {
					t.Fatalf("%s: %+v", script, r)
				}
			}
			for _, file := range []string{"scp-download", "batch-download", "drive-download", "tree/file", "scp-tree/file"} {
				data, err := os.ReadFile(filepath.Join(work, filepath.FromSlash(file)))
				if err != nil || string(data) != string(payload) {
					t.Fatalf("%s binary mismatch: %q %v", file, data, err)
				}
			}
		})
	}
}

func TestManagedSFTPPartialDownloadDoesNotCommit(t *testing.T) {
	c := newManagedSSHTestClient(t)
	fault := &failedSFTPRead{}
	c.sshRun = func(ctx context.Context, _ vbox.StartOptions, in io.Reader, out, _ io.Writer) (int, error) {
		handlers := sftp.InMemHandler()
		handlers.FileGet = fault
		server := sftp.NewRequestServer(sftpFixturePipe{in, out}, handlers)
		defer server.Close()
		err := server.Serve()
		if errors.Is(err, io.EOF) {
			err = nil
		}
		return 0, err
	}
	file := filepath.Join(c.options().WorkDir, "existing")
	if err := os.WriteFile(file, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.options().WorkDir, "source"), make([]byte, 65536), 0600); err != nil {
		t.Fatal(err)
	}
	r := runManagedScript(t, c, "grant ssh fixture:22; sftp -b - fixture", "put source broken\nget broken existing\n")
	if r.ExitCode != 1 || fault.bytes.Load() == 0 {
		t.Fatalf("failure not injected after data transfer: %+v bytes=%d", r, fault.bytes.Load())
	}
	data, err := os.ReadFile(file)
	if err != nil || string(data) != "original" {
		t.Fatalf("partial download committed: %q %v", data, err)
	}
	parts, _ := filepath.Glob(filepath.Join(c.options().WorkDir, ".aic-sftp-*.part"))
	if len(parts) != 0 {
		t.Fatalf("partial files leaked: %v", parts)
	}
}
