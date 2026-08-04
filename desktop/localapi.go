package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// LocalAPI 是桌面壳的本地 HTTP 服务（平台页面经 local_code 通道调用的入口）：
//
// 平台窗口 URL 携带 ?local_code={port}.{code}，页面（aic ui env.js）存入
// localStorage，local_handler.js 据此构造桥对象（window.localHandler）。
// 所有 API 请求须带 x-aic-code 头，code 由桌面随机生成（32 hex），生命周期
// = 桌面进程：启动时生成存内存，退出即消失，无过期/作废逻辑；
// 连续 5 次校验失败锁定 1 分钟（防暴力）。
//
// 安全边界：
//   - 仅监听 127.0.0.1 随机端口（不对外）
//   - Origin 白名单：仅平台源响应 CORS 头（浏览器层拦截其它源）
//   - Private Network Access 预检响应（https 页面 → 127.0.0.1 放行）
//   - 路径白名单：仅 /api/* 下明确注册的端点
type LocalAPI struct {
	mu      sync.Mutex
	app     *App
	srv     *http.Server
	port    int
	code    string
	failCnt int
	lockEnd time.Time
}

// defaultOrigins 是允许跨域访问本地服务的平台源（可扩展）。
func defaultOrigins() []string {
	return []string{
		"https://ivec.ai",
		"http://localhost:4000",
		"http://127.0.0.1:4000",
	}
}

func newLocalAPI(app *App) (*LocalAPI, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	l := &LocalAPI{
		app:  app,
		code: hex.EncodeToString(buf),
	}
	return l, nil
}

// Start 监听 127.0.0.1 随机端口并启动服务。
func (l *LocalAPI) Start() error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	l.port = ln.Addr().(*net.TCPAddr).Port

	mux := http.NewServeMux()
	mux.HandleFunc("/api/ping", l.handlePing)
	mux.HandleFunc("/api/get-config", l.handleJSON(func() any {
		cfg, _ := l.app.GetConfig()
		return cfg
	}, nil))
	mux.HandleFunc("/api/set-config", l.handleJSON(nil, func(v any) error {
		return l.app.SaveConfig(toConfig(v))
	}))
	mux.HandleFunc("/api/bind", l.handleBind)
	mux.HandleFunc("/api/host-status", l.handleJSON(func() any {
		return l.app.HostStatusQuery()
	}, nil))
	mux.HandleFunc("/api/host-log", l.handleJSON(func() any {
		return l.app.HostLog()
	}, nil))
	mux.HandleFunc("/api/host-start", l.handleHostStart)
	mux.HandleFunc("/api/host-stop", l.handleJSON(nil, func(v any) error {
		l.app.StopHost()
		return nil
	}))

	l.srv = &http.Server{Handler: l.withSecurity(mux)}
	go func() { _ = l.srv.Serve(ln) }()
	l.app.emitLog(fmt.Sprintf("local api listening on 127.0.0.1:%d (local_code=%s)", l.port, l.LocalCodeParam()))
	return nil
}

// LocalCodeParam 返回页面 URL 参数值：{port}.{code}。
func (l *LocalAPI) LocalCodeParam() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return fmt.Sprintf("%d.%s", l.port, l.code)
}

// Stop 关闭服务（应用退出时调用）。
func (l *LocalAPI) Stop() {
	if l.srv != nil {
		_ = l.srv.Close()
	}
}

// ---- 安全中间件 ----

func (l *LocalAPI) withSecurity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		allowed := ""
		for _, o := range defaultOrigins() {
			if o == origin {
				allowed = o
				break
			}
		}
		// 预检：仅白名单 origin 响应 CORS + PNA 头
		if r.Method == http.MethodOptions {
			if allowed == "" {
				http.Error(w, "forbidden origin", http.StatusForbidden)
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", allowed)
			w.Header().Set("Access-Control-Allow-Private-Network", "true")
			w.Header().Set("Access-Control-Allow-Headers", "x-aic-code, content-type")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if allowed != "" {
			w.Header().Set("Access-Control-Allow-Origin", allowed)
			w.Header().Set("Access-Control-Allow-Private-Network", "true")
		}
		// /api/ping 无需凭证（探测用）
		if r.URL.Path != "/api/ping" && !l.checkCode(r) {
			http.Error(w, "invalid or expired local code", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// checkCode 校验 x-aic-code：存在、未过期、未锁定；失败计数达 5 次锁 1 分钟。
func (l *LocalAPI) checkCode(r *http.Request) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if now.Before(l.lockEnd) {
		return false
	}
	if r.Header.Get("x-aic-code") != l.code {
		l.failCnt++
		if l.failCnt >= 5 {
			l.lockEnd = now.Add(time.Minute)
			l.failCnt = 0
		}
		return false
	}
	l.failCnt = 0
	return true
}

// ---- handlers ----

func (l *LocalAPI) handlePing(w http.ResponseWriter, _ *http.Request) {
	_, _ = w.Write([]byte("pong"))
}

// handleJSON 统一 JSON 响应包装：无参方法 / 单参方法。
func (l *LocalAPI) handleJSON(get func() any, set func(any) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && get != nil {
			writeJSON(w, get())
			return
		}
		if r.Method == http.MethodPost && set != nil {
			var v any
			if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
				http.Error(w, "invalid json", http.StatusBadRequest)
				return
			}
			if err := set(v); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			writeJSON(w, map[string]bool{"ok": true})
			return
		}
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleBind 绑定设备：保存配置并自动启动 host 会话。
func (l *LocalAPI) handleBind(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Host       string `json:"host"`
		Credential string `json:"credential"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Credential) == "" {
		http.Error(w, "credential is empty", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Host) == "" {
		req.Host = defaultConfig().Host
	}
	if err := l.app.SaveConfig(AppConfig{Host: req.Host, Credential: req.Credential}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := l.app.StartHost(req.Host, req.Credential); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "host": req.Host})
}

// handleHostStart 启动 host 会话（可携带覆盖配置）。
func (l *LocalAPI) handleHostStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Host       string `json:"host"`
		Credential string `json:"credential"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	hostURL, credential := req.Host, req.Credential
	if credential == "" {
		cfg, _ := l.app.GetConfig()
		hostURL, credential = cfg.Host, cfg.Credential
	}
	if err := l.app.StartHost(hostURL, credential); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func writeJSON(w http.ResponseWriter, v any) {
	_ = json.NewEncoder(w).Encode(v)
}

// toConfig 将任意 JSON 值安全转换为 AppConfig（字段白名单由 json tag 保证）。
func toConfig(v any) AppConfig {
	b, _ := json.Marshal(v)
	var cfg AppConfig
	_ = json.Unmarshal(b, &cfg)
	return cfg
}
