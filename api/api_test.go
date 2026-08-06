package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/vigo"
)

// randCode 生成随机校验码（测试环境，等价 cfg 的 newCode）。
func randCode() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// initTestAPI 初始化测试用本地服务（配置读写走默认路径）。
func initTestAPI(t *testing.T) {
	t.Helper()
	cfg.Global = cfg.NewOptions()
	if cfg.Global.Code == "" {
		cfg.Global.Code = randCode() // NewOptions 不再预生成，测试环境手动补（同 Load 语义）
	}
	mu.Lock()
	failCnt = 0
	lockEnd = time.Time{}
	mu.Unlock()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Global.SetPort(ln.Addr().(*net.TCPAddr).Port)
	srv, err := vigo.NewServer(vigo.WithHost("127.0.0.1"), vigo.WithPort(0), vigo.WithListener(ln))
	if err != nil {
		t.Fatal(err)
	}
	// 与根包一致的挂载方式（/api 前缀由 Extend 拼接）
	root := vigo.NewRouter()
	root.Extend("/api", Router)
	srv.SetRouter(root)
	go func() { _ = srv.Run() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
}

func req(t *testing.T, method, path, code string, body string) (int, string) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	q, err := http.NewRequest(method, fmt.Sprintf("http://127.0.0.1:%d%s", cfg.Global.Port(), path), r)
	if err != nil {
		t.Fatal(err)
	}
	if code != "" {
		q.Header.Set("x-aic-code", code)
	}
	resp, err := http.DefaultClient.Do(q)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestLocalAPIPing(t *testing.T) {
	initTestAPI(t)
	status, body := req(t, "GET", "/api/ping", "", "")
	if status != http.StatusOK || body != "pong" {
		t.Fatalf("ping = %d %q", status, body)
	}
}

func TestLocalAPICodeRequired(t *testing.T) {
	initTestAPI(t)
	// 无 code → 401
	status, _ := req(t, "GET", "/api/get_config", "", "")
	if status != http.StatusUnauthorized {
		t.Fatalf("no code: got %d, want 401", status)
	}
	// 错误 code → 401
	status, _ = req(t, "GET", "/api/get_config", "wrong", "")
	if status != http.StatusUnauthorized {
		t.Fatalf("wrong code: got %d, want 401", status)
	}
}

func TestLocalAPICorrectCode(t *testing.T) {
	initTestAPI(t)
	status, body := req(t, "GET", "/api/get_config", cfg.Global.Code, "")
	if status != http.StatusOK {
		t.Fatalf("correct code: got %d, want 200 (body %s)", status, body)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	if v["host"] == "" {
		t.Fatalf("config host empty")
	}
}

func TestLocalAPIFailLock(t *testing.T) {
	initTestAPI(t)
	// 连续 5 次错误 → 锁定 1 分钟
	for i := 0; i < 5; i++ {
		status, _ := req(t, "GET", "/api/get_config", "bad", "")
		if status != http.StatusUnauthorized {
			t.Fatalf("attempt %d: got %d, want 401", i, status)
		}
	}
	// 锁定期间正确 code 也被拒
	status, _ := req(t, "GET", "/api/get_config", cfg.Global.Code, "")
	if status != http.StatusUnauthorized {
		t.Fatalf("locked: correct code got %d, want 401", status)
	}
	// 锁定到期后恢复（缩短锁定时长验证）
	mu.Lock()
	lockEnd = time.Now().Add(-time.Second)
	mu.Unlock()
	status, _ = req(t, "GET", "/api/get_config", cfg.Global.Code, "")
	if status != http.StatusOK {
		t.Fatalf("after unlock: got %d, want 200", status)
	}
}

func TestLocalAPICORS(t *testing.T) {
	initTestAPI(t)
	// 白名单 origin 预检 → 204 + PNA 头
	q, _ := http.NewRequest(http.MethodOptions, fmt.Sprintf("http://127.0.0.1:%d/api/get_config", cfg.Global.Port()), nil)
	q.Header.Set("Origin", "https://ivec.ai")
	resp, err := http.DefaultClient.Do(q)
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
	q2, _ := http.NewRequest(http.MethodOptions, fmt.Sprintf("http://127.0.0.1:%d/api/get_config", cfg.Global.Port()), nil)
	q2.Header.Set("Origin", "https://evil.example.com")
	resp2, err := http.DefaultClient.Do(q2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusForbidden {
		t.Fatalf("evil origin: got %d, want 403", resp2.StatusCode)
	}
}

func TestCodeFormat(t *testing.T) {
	initTestAPI(t)
	if cfg.Global.Code == "" {
		t.Fatal("code should be generated")
	}
	// code 是纯秘钥（可配置/随机），不再携带端口与分隔符
	if strings.ContainsAny(cfg.Global.Code, ".\n\t ") {
		t.Fatalf("code must be a plain secret (no port/separators): %q", cfg.Global.Code)
	}
}

// set-config 白名单：host / work_dir / exec_timeout 可写（host 变更会重启会话）；
// key 不走 set_config（只走 bind），body 中的 credential 不得被持久化。
// work_dir 保存时校验有效性并展开为真实绝对路径（~ 展开、不存在 400）。
func TestLocalAPISetConfigWhitelist(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // 隔离配置文件（darwin UserConfigDir 用 HOME）
	initTestAPI(t)
	wd := t.TempDir()
	body := fmt.Sprintf(`{"work_dir":%q,"credential":"leak","exec_timeout":"5m","host":"http://x:1"}`, wd)
	status, resp := req(t, "POST", "/api/set_config", cfg.Global.Code, body)
	if status != http.StatusOK {
		t.Fatalf("set-config = %d %q", status, resp)
	}
	o, err := cfg.LoadFile()
	if err != nil {
		t.Fatal(err)
	}
	if o.WorkDir != wd || o.ExecTimeout != "5m" || o.Host != "http://x:1" {
		t.Fatalf("whitelist fields not saved: %+v", o)
	}
	if o.Key != "" {
		t.Fatalf("credential persisted via set_config: %+v", o)
	}
	// 非法 exec_timeout → 400
	status, _ = req(t, "POST", "/api/set_config", cfg.Global.Code, `{"exec_timeout":"bogus"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("bad timeout = %d, want 400", status)
	}
	// 无效 work_dir（不存在）→ 400
	status, resp = req(t, "POST", "/api/set_config", cfg.Global.Code, `{"work_dir":"/definitely-not-exist-xyz"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("bad work_dir = %d %q, want 400", status, resp)
	}
	// ~ 展开：~/subdir → $HOME/subdir（绝对路径落盘）
	sub := filepath.Join(os.Getenv("HOME"), "subdir")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	status, resp = req(t, "POST", "/api/set_config", cfg.Global.Code, `{"work_dir":"~/subdir"}`)
	if status != http.StatusOK {
		t.Fatalf("set-config ~/subdir = %d %q", status, resp)
	}
	o, err = cfg.LoadFile()
	if err != nil {
		t.Fatal(err)
	}
	if o.WorkDir != sub {
		t.Fatalf("work_dir not expanded: %q, want %q", o.WorkDir, sub)
	}
}

// unbind：清除凭证但保留 host/运行参数。
func TestLocalAPIUnbind(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	initTestAPI(t)
	if err := cfg.Save(&cfg.Options{Host: "http://x:1", Key: "h.1.s.u", WorkDir: "/w"}); err != nil {
		t.Fatal(err)
	}
	status, resp := req(t, "POST", "/api/unbind", cfg.Global.Code, "{}")
	if status != http.StatusOK {
		t.Fatalf("unbind = %d %q", status, resp)
	}
	o, err := cfg.LoadFile()
	if err != nil {
		t.Fatal(err)
	}
	if o.Key != "" {
		t.Fatalf("credential not cleared: %+v", o)
	}
	if o.Host != "http://x:1" || o.WorkDir != "/w" {
		t.Fatalf("other fields lost: %+v", o)
	}
}

// get_log：日志文件不存在返回空串 JSON。
func TestLocalAPIGetLog(t *testing.T) {
	initTestAPI(t)
	status, body := req(t, "GET", "/api/get_log", cfg.Global.Code, "")
	if status != http.StatusOK {
		t.Fatalf("get_log = %d", status)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("get_log not a JSON object: %q", body)
	}
}
