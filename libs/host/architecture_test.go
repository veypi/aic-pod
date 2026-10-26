package host

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/vbox"
)

func TestUnifiedExecDenialSkipsCommandSideEffects(t *testing.T) {
	withGlobal(t, func(o *cfg.Options) { o.ExecPolicy = cfg.PolicyDeny })
	c, _ := testClient(t)
	var httpCalls, sshCalls, gitCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		httpCalls.Add(1)
	}))
	defer server.Close()
	c.sshRun = func(context.Context, vbox.StartOptions, io.Reader, io.Writer, io.Writer) (int, error) {
		sshCalls.Add(1)
		return 0, nil
	}
	c.gitRun = func(context.Context, vbox.StartOptions, io.Reader, io.Writer, io.Writer) (int, error) {
		gitCalls.Add(1)
		return 0, nil
	}
	marker := filepath.Join(c.options().WorkDir, "native-marker")
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip(err)
	}
	beforeFSRules := len(c.perms.base.fsRules)
	scripts := []string{
		"printf builtin",
		"grant fs /tmp",
		"mcp add denyprobe --url " + server.URL,
		"curl -sS " + server.URL,
		"ssh fixture",
		"git ls-remote fixture",
		sh + " -c 'printf x > " + marker + "'",
	}
	for _, script := range scripts {
		result := runManagedScript(t, c, script, "")
		if result.ExitCode != 126 {
			t.Fatalf("%s: result=%+v, want unified exec denial", script, result)
		}
		if result.Stdout != "" {
			t.Fatalf("%s: denial produced stdout %q", script, result.Stdout)
		}
	}
	if len(c.perms.base.fsRules) != beforeFSRules {
		t.Fatal("grant changed rules after unified exec denial")
	}
	if c.serviceRegistered("denyprobe") {
		t.Fatal("MCP add registered a service after unified exec denial")
	}
	if httpCalls.Load() != 0 {
		t.Fatalf("curl reached the HTTP server %d times", httpCalls.Load())
	}
	if sshCalls.Load() != 0 || gitCalls.Load() != 0 {
		t.Fatalf("SSH/Git implementations ran: ssh=%d git=%d", sshCalls.Load(), gitCalls.Load())
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("native command created %s after unified exec denial", marker)
	}
}

func newConfinedTestClient(t *testing.T) *Client {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	c := New(Options{Key: "host_1.1.secret.owner", WorkDir: t.TempDir(), NoSandbox: false})
	c.hostID = "host_1"
	c.uid = "owner"
	c.kTool = "test-key"
	c.sessionRoot = t.TempDir()
	if c.initErr != nil {
		t.Fatal(c.initErr)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestNativeNetCapabilityFailureIsLocal(t *testing.T) {
	withGlobal(t, func(o *cfg.Options) {
		o.NetPolicy = cfg.PolicyOpen
		o.NetRules = []string{"deny:169.254.169.254:*"}
	})
	c := newConfinedTestClient(t)
	var httpCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		httpCalls.Add(1)
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	if result := runManagedScript(t, c, "curl -sS "+server.URL, ""); result.ExitCode != 0 || result.Stdout != "ok" || httpCalls.Load() != 1 {
		t.Fatalf("independent curl failed: %+v calls=%d", result, httpCalls.Load())
	}
	marker := filepath.Join(c.options().WorkDir, "unsupported-native")
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip(err)
	}
	result := runManagedScript(t, c, sh+" -c 'printf x > "+marker+"'", "")
	if result.ExitCode != 126 {
		t.Fatalf("native capability gap did not reject only the native command: %+v", result)
	}
	if !strings.Contains(result.Stderr, "selective network rule") {
		t.Fatalf("native error does not expose the capability gap: %+v", result)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("unsupported native command started and created %s", marker)
	}
	if httpCalls.Load() != 1 {
		t.Fatalf("native failure affected independent curl: calls=%d", httpCalls.Load())
	}
}
