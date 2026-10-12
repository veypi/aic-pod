package host

// pullpush_test.go — pull/push 的 httptest 回放：模拟 /fs/cloud 面
//（文件直出 / 目录 JSON 树 / PUT 写），验证流式落盘、覆盖语义、目录递归与拒绝、
// Bearer 注入与明文护栏。

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	podcfg "github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/execution"
	"github.com/veypi/vsh/commands"
	"github.com/veypi/vsh/policy"
)

// fakeCloud 模拟 /fs/cloud 面的最小服务（内存文件树）。
type fakeCloud struct {
	mu       sync.Mutex
	files    map[string][]byte
	lastAuth string
	puts     int
}

func (f *fakeCloud) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.lastAuth = r.Header.Get("Authorization")
	f.mu.Unlock()
	p := strings.TrimPrefix(r.URL.Path, "/fs/cloud")
	switch r.Method {
	case http.MethodGet:
		if strings.HasSuffix(p, "/dir") || strings.HasPrefix(p, "/dir/") && r.URL.Query().Get("depth") != "" {
			// 目录：递归 JSON 树
			w.Header().Set("X-UFS-Dir", "true")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name": "dir", "dir": true,
				"items": []map[string]any{
					{"name": "a.txt", "size": 1},
					{"name": "sub", "dir": true, "items": []map[string]any{{"name": "b.txt", "size": 2}}},
				},
			})
			return
		}
		f.mu.Lock()
		data, ok := f.files[p]
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("X-UFS-Dir", "false")
		_, _ = w.Write(data)
	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.files[p] = body
		f.puts++
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func setupPullPush(t *testing.T, execRules []string) (*Client, *fakeCloud, *httptest.Server) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	old := podcfg.Global
	t.Cleanup(func() { podcfg.Global = old })
	podcfg.Global = podcfg.NewOptions()
	podcfg.Global.ExecRules = execRules

	fake := &fakeCloud{files: map[string][]byte{
		"/u/u1/file.txt":      []byte("hello"),
		"/u/u1/dir/a.txt":     []byte("A"),
		"/u/u1/dir/sub/b.txt": []byte("BB"),
	}}
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(server.Close)

	payload, _ := json.Marshal(map[string]any{"uid": "u1", "sub": "u1", "ver": 1, "type": "static", "mod": "limited"})
	composite := "testhost32." + base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256"}`)) +
		"." + base64.RawURLEncoding.EncodeToString(payload) + ".sig.test-secret"
	c := New(Options{Key: composite, Host: server.URL, WorkDir: t.TempDir(), RTC: false})
	t.Cleanup(func() { c.Close() })
	return c, fake, server
}

func runPullPush(t *testing.T, c *Client, name string, args ...string) (string, error) {
	t.Helper()
	fsys, err := execution.NewHostFS(execution.HostFSConfig{Backing: OSVFS{}})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	inv := commands.NewInvocation(&commands.InvocationOptions{
		Args:       args,
		Cwd:        "/",
		Stdout:     &out,
		FileSystem: fsys,
		Policy:     policy.NewStatic(nil),
	})
	fn := c.cmdPull
	if name == "push" {
		fn = c.cmdPush
	}
	err = fn(context.Background(), inv)
	return out.String(), err
}

func TestPullFileOverwriteAndBearer(t *testing.T) {
	c, fake, _ := setupPullPush(t, nil)
	dst := filepath.Join(t.TempDir(), "out.txt")
	if err := os.WriteFile(dst, []byte("old-content-longer"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runPullPush(t, c, "pull", "/u/u1/file.txt", dst)
	if err != nil {
		t.Fatalf("pull: %v (%s)", err, out)
	}
	data, _ := os.ReadFile(dst)
	if string(data) != "hello" {
		t.Fatalf("overwrite semantics broken: %q", data)
	}
	if fake.lastAuth == "" || !strings.HasPrefix(fake.lastAuth, "Bearer ") {
		t.Fatalf("Bearer not injected: %q", fake.lastAuth)
	}
}

func TestPullDirRecursive(t *testing.T) {
	c, _, _ := setupPullPush(t, nil)
	root := t.TempDir()
	dst := filepath.Join(root, "dir")
	out, err := runPullPush(t, c, "pull", "/u/u1/dir/", dst)
	if err != nil {
		t.Fatalf("pull dir: %v (%s)", err, out)
	}
	a, _ := os.ReadFile(filepath.Join(dst, "a.txt"))
	b, _ := os.ReadFile(filepath.Join(dst, "sub", "b.txt"))
	if string(a) != "A" || string(b) != "BB" {
		t.Fatalf("dir pull mismatch: a=%q b=%q", a, b)
	}
}

func TestPushFileAndDirRejected(t *testing.T) {
	c, fake, _ := setupPullPush(t, nil)
	src := filepath.Join(t.TempDir(), "up.bin")
	if err := os.WriteFile(src, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runPullPush(t, c, "push", src, "/u/u1/up.bin")
	if err != nil {
		t.Fatalf("push: %v (%s)", err, out)
	}
	if string(fake.files["/u/u1/up.bin"]) != "payload" {
		t.Fatalf("pushed content mismatch: %q", fake.files["/u/u1/up.bin"])
	}
	// 目录 v1 拒绝
	if _, err := runPullPush(t, c, "push", t.TempDir(), "/u/u1/x"); err == nil {
		t.Fatal("push directory should be rejected")
	}
}

func TestPullPushExecRuleGate(t *testing.T) {
	c, _, _ := setupPullPush(t, []string{"deny:pull", "deny:push"})
	if _, err := runPullPush(t, c, "pull", "/u/u1/file.txt", filepath.Join(t.TempDir(), "x")); err == nil {
		t.Fatal("pull should be denied by exec rules")
	}
	if _, err := runPullPush(t, c, "push", "x", "/u/u1/y"); err == nil {
		t.Fatal("push should be denied by exec rules")
	}
}

func TestCloudFSPlaintextGuard(t *testing.T) {
	if _, err := newCloudFS("http://example.com", "cred"); err == nil {
		t.Fatal("plaintext http to non-loopback must be refused")
	}
	if _, err := newCloudFS("http://127.0.0.1:4000", "cred"); err != nil {
		t.Fatalf("loopback http should be allowed: %v", err)
	}
	if _, err := newCloudFS("https://ivec-ai.com", "cred"); err != nil {
		t.Fatalf("https should be allowed: %v", err)
	}
}
