package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/veypi/aic-pod/sdk/host"
)

// newTestLocalAPI 创建测试用 LocalAPI（App 不连 host，配置读写走默认路径）。
func newTestLocalAPI(t *testing.T) *LocalAPI {
	t.Helper()
	app := &App{}
	api, err := newLocalAPI(app)
	if err != nil {
		t.Fatal(err)
	}
	app.local = api
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
	status, _ := api.req(t, "GET", "/api/get-config", "", "")
	if status != http.StatusUnauthorized {
		t.Fatalf("no code: got %d, want 401", status)
	}
	// 错误 code → 401
	status, _ = api.req(t, "GET", "/api/get-config", "wrong", "")
	if status != http.StatusUnauthorized {
		t.Fatalf("wrong code: got %d, want 401", status)
	}
}

func TestLocalAPICorrectCode(t *testing.T) {
	api := newTestLocalAPI(t)
	status, body := api.req(t, "GET", "/api/get-config", api.code, "")
	if status != http.StatusOK {
		t.Fatalf("correct code: got %d, want 200 (body %s)", status, body)
	}
	var cfg AppConfig
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
		status, _ := api.req(t, "GET", "/api/get-config", "bad", "")
		if status != http.StatusUnauthorized {
			t.Fatalf("attempt %d: got %d, want 401", i, status)
		}
	}
	// 锁定期间正确 code 也被拒
	status, _ := api.req(t, "GET", "/api/get-config", api.code, "")
	if status != http.StatusUnauthorized {
		t.Fatalf("locked: correct code got %d, want 401", status)
	}
	// 锁定到期后恢复（缩短锁定时长验证）
	api.mu.Lock()
	api.lockEnd = time.Now().Add(-time.Second)
	api.mu.Unlock()
	status, _ = api.req(t, "GET", "/api/get-config", api.code, "")
	if status != http.StatusOK {
		t.Fatalf("after unlock: got %d, want 200", status)
	}
}

func TestLocalAPICORS(t *testing.T) {
	api := newTestLocalAPI(t)
	// 白名单 origin 预检 → 204 + PNA 头
	req, _ := http.NewRequest(http.MethodOptions, fmt.Sprintf("http://127.0.0.1:%d/api/get-config", api.port), nil)
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
	req2, _ := http.NewRequest(http.MethodOptions, fmt.Sprintf("http://127.0.0.1:%d/api/get-config", api.port), nil)
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

// set-config 白名单：仅 work_dir/device_name/exec_timeout 可写；
// host/credential 即使在 body 中也不得被持久化。
func TestLocalAPISetConfigWhitelist(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // 隔离配置文件（darwin UserConfigDir 用 HOME）
	api := newTestLocalAPI(t)
	body := `{"work_dir":"/w","device_name":"box","exec_timeout":"5m","host":"http://evil","credential":"leak"}`
	status, resp := api.req(t, "POST", "/api/set-config", api.code, body)
	if status != http.StatusOK {
		t.Fatalf("set-config = %d %q", status, resp)
	}
	cfg, err := host.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WorkDir != "/w" || cfg.DeviceName != "box" || cfg.ExecTimeout != "5m" {
		t.Fatalf("whitelist fields not saved: %+v", cfg)
	}
	if cfg.Host != host.DefaultHost || cfg.Credential != "" {
		t.Fatalf("non-whitelist fields persisted: %+v", cfg)
	}
	// 非法 exec_timeout → 400
	status, _ = api.req(t, "POST", "/api/set-config", api.code, `{"exec_timeout":"bogus"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("bad timeout = %d, want 400", status)
	}
}
