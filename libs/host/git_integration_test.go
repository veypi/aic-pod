package host

// managed git 端到端：内存 sshd（loopback）+ 真实 git-upload-pack /
// git-receive-pack。验证 vsh 脚本里的 git 传输在不暴露任何私钥给沙箱的
// 前提下走通 clone/pull/push/ls-remote，授权域与 managed ssh 一致（ssh）。
import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/execution"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// startGitPackSSHFixture 与 startManagedSSHFixture 同构，但 exec 通道实现为
// 真实的 git-upload-pack / git-receive-pack（命令形态 git-upload-pack '<path>'）。
func startGitPackSSHFixture(t *testing.T, home string) (string, *atomic.Int32) {
	t.Helper()
	if _, err := trustedSSHBinary(); err != nil {
		t.Skip(err)
	}
	for _, tool := range []string{"git-upload-pack", "git-receive-pack"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%v", err)
		}
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
							if req.Type != "exec" {
								req.Reply(false, nil)
								continue
							}
							var value struct{ Value string }
							_ = ssh.Unmarshal(req.Payload, &value)
							tool, repoPath, ok := parseGitPackCommand(value.Value)
							if !ok {
								req.Reply(true, nil)
								ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{2}))
								return
							}
							req.Reply(true, nil)
							exe, _ := exec.LookPath(tool)
							cmd := exec.Command(exe, repoPath)
							cmd.Stdin = ch
							cmd.Stdout = ch
							cmd.Stderr = ch.Stderr()
							status := uint32(0)
							if err := cmd.Run(); err != nil {
								status = 1
								if ee, ok := err.(*exec.ExitError); ok {
									status = uint32(ee.ExitCode())
								}
							}
							ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
							return
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

// parseGitPackCommand 解析 git 经 ssh 下发的远端命令：git-upload-pack '<path>'
// 或 git-receive-pack '<path>'（单引号包裹，内嵌单引号以 '\” 转义）。
func parseGitPackCommand(raw string) (string, string, bool) {
	for _, tool := range []string{"git-upload-pack", "git-receive-pack"} {
		prefix := tool + " '"
		if strings.HasPrefix(raw, prefix) && strings.HasSuffix(raw, "'") {
			p := raw[len(prefix) : len(raw)-1]
			return tool, strings.ReplaceAll(p, `'\''`, "'"), true
		}
	}
	return "", "", false
}

// runGitScript 与 runManagedScript 同构，但 PATH 保留系统目录——git 不是注册
// 命令，vsh 需要经 PATH 解析到原生二进制后再被 managed 层拦截。HOME 仍不可信，
// 证明受管路径不依赖脚本环境。
func runGitScript(t *testing.T, c *Client, script string) *execution.ExecResult {
	t.Helper()
	e, err := c.engine()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := e.Exec(ctx, execution.ExecRequest{
		SessionKey: "git-test", Script: script,
		WorkDir: hostCanonical(c.options().WorkDir),
		Env: map[string]string{
			"HOME": "/untrusted", "PATH": "/usr/bin:/bin:/usr/local/bin:/opt/homebrew/bin",
			"SSH_AUTH_SOCK": "/untrusted", "SSH_ASKPASS": "/untrusted",
		},
		GrantApproved: true,
	})
	if err != nil {
		t.Fatalf("%s: %v", script, err)
	}
	return r
}

// runOSGit 测试侧直接跑系统 git（不经过被测路径），用于造仓库与验证远端状态。
func runOSGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip(err)
	}
	cmd := exec.Command(git, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_CONFIG_NOSYSTEM=1", "HOME="+os.Getenv("HOME"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("os git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func seedGitRemote(t *testing.T) (bare string) {
	t.Helper()
	root := t.TempDir()
	work := filepath.Join(root, "work")
	runOSGit(t, root, "-c", "init.defaultBranch=main", "init", work)
	if err := os.WriteFile(filepath.Join(work, "hello.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runOSGit(t, work, "add", ".")
	runOSGit(t, work, "commit", "-m", "init")
	bare = filepath.Join(root, "remote.git")
	runOSGit(t, root, "clone", "--bare", work, bare)
	return bare
}

func TestManagedGitSSHIntegration(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("managed git 未接入 windows")
	}
	if _, err := trustedGitBinary(); err != nil {
		t.Skip(err)
	}
	c := newManagedSSHTestClient(t)
	// cmd 域默认 deny：验证 https/本地子命令交还原生路径后的确定性结果
	//（不依赖外部网络）。managed 传输不经 cmd 域，不受影响。
	cfg.Global.ExecPolicy = cfg.PolicyDeny
	publishGlobal(t, c.perms)
	home, _ := os.UserHomeDir()
	address, calls := startGitPackSSHFixture(t, home)
	bare := seedGitRemote(t)
	remoteURL := "fixture:" + bare

	// 未授权：126 + grant 指引，且不产生任何连接。
	if r := runGitScript(t, c, "git clone "+remoteURL+" dst"); r.ExitCode != 126 ||
		!strings.Contains(r.Stderr, "grant ssh "+address) || calls.Load() != 0 {
		t.Fatalf("denial %+v connections=%d", r, calls.Load())
	}
	// https / 本地子命令不受 managed 接管：走原生 cmd 闸。
	if r := runGitScript(t, c, "git clone https://example.com/x.git y"); r.ExitCode != 126 ||
		!strings.Contains(r.Stderr, "grant cmd git") {
		t.Fatalf("https passthrough %+v", r)
	}
	if r := runGitScript(t, c, "git status"); r.ExitCode != 126 ||
		!strings.Contains(r.Stderr, "grant cmd git") {
		t.Fatalf("local passthrough %+v", r)
	}
	// 选项白名单：破坏性/危险选项 fail-closed，不接触网络。
	if r := runGitScript(t, c, "git push --force"); r.ExitCode != 2 || !strings.Contains(r.Stderr, "破坏性") {
		t.Fatalf("force reject %+v", r)
	}
	if r := runGitScript(t, c, "git fetch --all"); r.ExitCode != 2 || !strings.Contains(r.Stderr, "多 remote") {
		t.Fatalf("fetch --all reject %+v", r)
	}

	// 授权后 clone。
	if r := runGitScript(t, c, "grant ssh "+address+"; git clone "+remoteURL+" dst"); r.ExitCode != 0 {
		t.Fatalf("clone %+v", r)
	}
	workDir := c.options().WorkDir
	if data, err := os.ReadFile(filepath.Join(workDir, "dst", "hello.txt")); err != nil || string(data) != "v1\n" {
		t.Fatalf("clone 内容: %q %v", data, err)
	}
	if r := runGitScript(t, c, "git ls-remote "+remoteURL); r.ExitCode != 0 ||
		!strings.Contains(r.Stdout, "refs/heads/main") {
		t.Fatalf("ls-remote %+v", r)
	}

	// 上游推进一格：pull 同步。
	upstream := filepath.Join(t.TempDir(), "upstream")
	runOSGit(t, "", "clone", bare, upstream)
	if err := os.WriteFile(filepath.Join(upstream, "v2.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runOSGit(t, upstream, "add", ".")
	runOSGit(t, upstream, "commit", "-m", "v2")
	runOSGit(t, upstream, "push", "origin", "main")
	if r := runGitScript(t, c, "cd dst; git pull"); r.ExitCode != 0 {
		t.Fatalf("pull %+v", r)
	}
	if data, err := os.ReadFile(filepath.Join(workDir, "dst", "v2.txt")); err != nil || string(data) != "v2\n" {
		t.Fatalf("pull 内容: %q %v", data, err)
	}

	// 本地提交后 push；缺省 remote 跟随分支配置（受管层显式化）。
	if err := os.WriteFile(filepath.Join(workDir, "dst", "v3.txt"), []byte("v3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runOSGit(t, filepath.Join(workDir, "dst"), "add", ".")
	runOSGit(t, filepath.Join(workDir, "dst"), "commit", "-m", "v3")
	if r := runGitScript(t, c, "cd dst; git push"); r.ExitCode != 0 {
		t.Fatalf("push %+v", r)
	}
	if out := runOSGit(t, bare, "log", "--format=%s", "-1", "main"); strings.TrimSpace(out) != "v3" {
		t.Fatalf("远端未更新: %q", out)
	}
	// fetch 显式 remote 名。
	if r := runGitScript(t, c, "cd dst; git fetch origin"); r.ExitCode != 0 {
		t.Fatalf("fetch %+v", r)
	}
}
