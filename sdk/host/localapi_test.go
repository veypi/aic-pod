package host

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeHost 是 LocalHost 的测试替身（不真正连接 NATS）。
type fakeHost struct {
	mu      sync.Mutex
	running bool
}

func (f *fakeHost) StartHost(cfg Config) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running = true
	return nil
}

func (f *fakeHost) StopHost() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running = false
}

func (f *fakeHost) Running() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running
}

// newTestLocalAPI 创建测试用 LocalAPI（fake host，配置读写走默认路径）。
func newTestLocalAPI(t *testing.T) *LocalAPI {
	t.Helper()
	api, err := NewLocalAPI(&fakeHost{}, "v0.0.0-test", NewRingBuffer(200), Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := api.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(api.Stop)
	return api
}

func (l *LocalAPI) req(t *testing.T, method, path, code string, body string) (int, string) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, fmt.Sprintf("http://127.0.0.1:%d%s", l.port, path), r)
	if err != nil {
		t.Fatal(err)
	}
	if code != "" {
		req.Header.Set("x-aic-code", code)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestLocalAPIPing(t *testing.T) {
	api := newTestLocalAPI(t)
	status, body := api.req(t, "GET", "/api/ping", "", "")
	if status != http.StatusOK || body != "pong" {
		t.Fatalf("ping = %d %q", status, body)
	}
}

func TestLocalAPICodeRequired(t *testing.T) {
	api := newTestLocalAPI(t)
	// 无 code → 401
	status, _ := api.req(t, "GET", "/api/get_config", "", "")
	if status != http.StatusUnauthorized {
		t.Fatalf("no code: got %d, want 401", status)
	}
	// 错误 code → 401
	status, _ = api.req(t, "GET", "/api/get_config", "wrong", "")
	if status != http.StatusUnauthorized {
		t.Fatalf("wrong code: got %d, want 401", status)
	}
}

func TestLocalAPICorrectCode(t *testing.T) {
	api := newTestLocalAPI(t)
	status, body := api.req(t, "GET", "/api/get_config", api.code, "")
	if status != http.StatusOK {
		t.Fatalf("correct code: got %d, want 200 (body %s)", status, body)
	}
	var cfg Config
	if err := json.Unmarshal([]byte(body), &cfg); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	if cfg.Host == "" {
		t.Fatalf("config host empty")
	}
}

func TestLocalAPIFailLock(t *testing.T) {
	api := newTestLocalAPI(t)
	// 连续 5 次错误 → 锁定 1 分钟
	for i := 0; i < 5; i++ {
		status, _ := api.req(t, "GET", "/api/get_config", "bad", "")
		if status != http.StatusUnauthorized {
			t.Fatalf("attempt %d: got %d, want 401", i, status)
		}
	}
	// 锁定期间正确 code 也被拒
	status, _ := api.req(t, "GET", "/api/get_config", api.code, "")
	if status != http.StatusUnauthorized {
		t.Fatalf("locked: correct code got %d, want 401", status)
	}
	// 锁定到期后恢复（缩短锁定时长验证）
	api.mu.Lock()
	api.lockEnd = time.Now().Add(-time.Second)
	api.mu.Unlock()
	status, _ = api.req(t, "GET", "/api/get_config", api.code, "")
	if status != http.StatusOK {
		t.Fatalf("after unlock: got %d, want 200", status)
	}
}

func TestLocalAPICORS(t *testing.T) {
	api := newTestLocalAPI(t)
	// 白名单 origin 预检 → 204 + PNA 头
	req, _ := http.NewRequest(http.MethodOptions, fmt.Sprintf("http://127.0.0.1:%d/api/get_config", api.port), nil)
	req.Header.Set("Origin", "https://ivec.ai")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("cors preflight: got %d, want 204", resp.StatusCode)
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "https://ivec.ai" {
		t.Fatalf("missing allow-origin: %q", resp.Header.Get("Access-Control-Allow-Origin"))
	}
	if resp.Header.Get("Access-Control-Allow-Private-Network") != "true" {
		t.Fatalf("missing PNA header")
	}
	// 非白名单 origin → 403
	req2, _ := http.NewRequest(http.MethodOptions, fmt.Sprintf("http://127.0.0.1:%d/api/get_config", api.port), nil)
	req2.Header.Set("Origin", "https://evil.example.com")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusForbidden {
		t.Fatalf("evil origin: got %d, want 403", resp2.StatusCode)
	}
}

func TestLocalCodeParamFormat(t *testing.T) {
	api := newTestLocalAPI(t)
	p := api.LocalCodeParam()
	idx := strings.IndexByte(p, '.')
	if idx <= 0 || idx == len(p)-1 {
		t.Fatalf("local_code format: %q", p)
	}
	if p[idx+1:] != api.code {
		t.Fatalf("code mismatch: %q vs %q", p[idx+1:], api.code)
	}
}

// set-config 白名单：host / work_dir / exec_timeout 可写（host 变更会重启会话）；
// key 不走 set_config（只走 bind），body 中的 credential 不得被持久化。
func TestLocalAPISetConfigWhitelist(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // 隔离配置文件（darwin UserConfigDir 用 HOME）
	api := newTestLocalAPI(t)
	body := `{"work_dir":"/w","credential":"leak","exec_timeout":"5m","host":"http://x:1"}`
	status, resp := api.req(t, "POST", "/api/set_config", api.code, body)
	if status != http.StatusOK {
		t.Fatalf("set-config = %d %q", status, resp)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WorkDir != "/w" || cfg.ExecTimeout != "5m" || cfg.Host != "http://x:1" {
		t.Fatalf("whitelist fields not saved: %+v", cfg)
	}
	if cfg.Key != "" {
		t.Fatalf("credential persisted via set_config: %+v", cfg)
	}
	// 非法 exec_timeout → 400
	status, _ = api.req(t, "POST", "/api/set_config", api.code, `{"exec_timeout":"bogus"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("bad timeout = %d, want 400", status)
	}
}

// unbind：清除凭证但保留 host/运行参数。
func TestLocalAPIUnbind(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	api := newTestLocalAPI(t)
	if err := SaveConfig(Config{Host: "http://x:1", Key: "h.1.s.u", WorkDir: "/w"}); err != nil {
		t.Fatal(err)
	}
	status, resp := api.req(t, "POST", "/api/unbind", api.code, "{}")
	if status != http.StatusOK {
		t.Fatalf("unbind = %d %q", status, resp)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Key != "" {
		t.Fatalf("credential not cleared: %+v", cfg)
	}
	if cfg.Host != "http://x:1" || cfg.WorkDir != "/w" {
		t.Fatalf("other fields lost: %+v", cfg)
	}
}

// get_log：无日志返回空串 JSON；RingBuffer 有内容时返回其内容。
func TestLocalAPIGetLog(t *testing.T) {
	api := newTestLocalAPI(t)
	status, body := api.req(t, "GET", "/api/get_log", api.code, "")
	if status != http.StatusOK {
		t.Fatalf("get_log = %d", status)
	}
	var s string
	if err := json.Unmarshal([]byte(body), &s); err != nil {
		t.Fatalf("get_log not a JSON string: %q", body)
	}
}

func TestHostsURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "https://ivec.ai/hosts"},
		{"https://ivec.ai", "https://ivec.ai/hosts"},
		{"http://localhost:4000", "http://localhost:4000/hosts"},
		{"http://localhost:4000/", "http://localhost:4000/hosts"},
		{"http://127.0.0.1:4000/rses/aiv", "http://127.0.0.1:4000/rses/aiv/hosts"},
		{"https://ivec.ai/hosts", "https://ivec.ai/hosts"},
		{"ivec.ai", "https://ivec.ai/hosts"},
		{"http://x:1/?q=1", "http://x:1/hosts"},
	}
	for _, c := range cases {
		if got := HostsURL(c.in); got != c.want {
			t.Errorf("HostsURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
