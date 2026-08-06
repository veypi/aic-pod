// LocalAPI 是本地管理 API（cli/desktop 共用，vigo 框架实现）：
// 127.0.0.1 随机端口 HTTP 服务，平台页面经 local_code 通道调用。
//
// 协议（aic ui local_handler.js）：local_code = "{port}.{code}"（cli 与桌面同格式，
// 插件为 "ext.{hex}"），所有 API 请求须带 x-aic-code 头。安全边界：
//   - 仅监听 127.0.0.1 随机端口（不对外）
//   - Origin 白名单：仅平台源响应 CORS 头（含 Private Network Access 预检）
//   - code 由进程随机生成（32 hex），生命周期 = 进程；连续 5 次校验失败锁 1 分钟
package host

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/veypi/vigo"
	"github.com/veypi/vigo/logv"
)

// LocalHost 是 LocalAPI 的宿主：host 会话生命周期。
// cli 用 Runner（sdk/host），desktop 用 App（Wails 壳）实现。
// 日志不经此接口——统一走 vigo/logv（get_log 从挂入 logv 的 RingBuffer 读取）。
type LocalHost interface {
	StartHost(cfg Config) error
	StopHost()
	Running() bool
	// OpenPlatformURL 应用内打开平台 URL（desktop 窗口跳转；cli 无窗口降级系统浏览器）。
	OpenPlatformURL(url string) error
	// ApplyConfig 应用新运行配置（保存设置后调用）：保留会话与 bg 任务，
	// 仅更新参数；NATS 地址变化时重连。
	ApplyConfig(cfg Config) error
}

// LocalAPI 本地管理服务。
// cfg 为启动时 flags 解析的有效配置（flag/env/文件/default），页面写操作
// （bind/unbind/set_config）同步更新内存态并持久化文件。
type LocalAPI struct {
	mu      sync.Mutex
	host    LocalHost
	version string
	log     *RingBuffer
	cfg     Config
	srv     *vigo.Application
	port    int
	code    string
	failCnt int
	lockEnd time.Time
}

// NewLocalAPI 创建 LocalAPI（不启动）。cfg 为启动解析的有效配置（get_config/
// get_status/start 的基准）；log 为日志缓冲（get_log 数据源，由调用方创建并挂入
// vigo/logv）；version 为客户端版本（get_status 返回）。
func NewLocalAPI(host LocalHost, version string, log *RingBuffer, cfg Config) (*LocalAPI, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	return &LocalAPI{
		host:    host,
		version: version,
		log:     log,
		cfg:     cfg.Normalize(),
		code:    hex.EncodeToString(buf),
	}, nil
}

// Start 监听 127.0.0.1 随机端口并启动服务。
func (l *LocalAPI) Start() error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	l.port = ln.Addr().(*net.TCPAddr).Port

	router := vigo.NewRouter()
	router.Use(l.security)
	// 全部端点 Any 注册：OPTIONS 预检由安全中间件短路，handler 内按方法分派
	for _, p := range []string{
		"/api/ping",
		"/api/get_config",
		"/api/set_config",
		"/api/bind",
		"/api/unbind",
		"/api/get_status",
		"/api/get_log",
		"/api/start",
		"/api/stop",
		"/api/open_url",
		"/api/open_platform",
			"/settings",
	} {
		router.Any(p, l.dispatch(p))
	}

	srv, err := vigo.NewServer(vigo.WithHost("127.0.0.1"), vigo.WithPort(0), vigo.WithListener(ln))
	if err != nil {
		ln.Close()
		return err
	}
	srv.SetRouter(router)
	l.srv = srv
	go func() { _ = srv.Run() }()
	logv.WithNoCaller.Info().Msgf("local api listening on 127.0.0.1:%d (local_code=%s)", l.port, l.LocalCodeParam())
	return nil
}

// Stop 关闭服务（应用退出时调用）。
func (l *LocalAPI) Stop() {
	if l.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = l.srv.Shutdown(ctx)
	}
}

// LocalCodeParam 返回页面 URL 参数值：{port}.{code}。
func (l *LocalAPI) LocalCodeParam() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return fmt.Sprintf("%d.%s", l.port, l.code)
}

// Code 返回纯 code（x-aic-code 头用，不含端口前缀）。
func (l *LocalAPI) Code() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.code
}

// Port 返回实际监听端口。
func (l *LocalAPI) Port() int {
	return l.port
}

// ---- 安全中间件 ----

// defaultOrigins 是默认允许跨域访问本地服务的平台源。
// "null" 为本地设置窗口（wails SetHTML，origin 为 null）——受 code 校验保护。
func defaultOrigins() []string {
	return []string{
		"https://ivec.ai",
		"http://localhost:4000",
		"http://127.0.0.1:4000",
		"null",
	}
}

// allowedOrigins 实际白名单 = 默认列表 + 当前有效配置 host 的 origin
//（启动 flag/env 注入的测试地址等自动放行）。
func (l *LocalAPI) allowedOrigins() []string {
	list := defaultOrigins()
	if cfg := l.loadEffective(); cfg.Host != "" {
		if u, err := url.Parse(cfg.Host); err == nil && u.Scheme != "" && u.Host != "" {
			origin := fmt.Sprintf("%s://%s", u.Scheme, u.Host)
			for _, o := range list {
				if o == origin {
					return list
				}
			}
			list = append(list, origin)
		}
	}
	return list
}

// security 是安全中间件：CORS 白名单 + OPTIONS 预检短路 + code 校验。
// 校验失败写响应后 x.Stop() 终止后续 handler。
func (l *LocalAPI) security(x *vigo.X) error {
	r := x.Request
	w := x.ResponseWriter()
	origin := r.Header.Get("Origin")
	allowed := ""
	for _, o := range l.allowedOrigins() {
		if o == origin {
			allowed = o
			break
		}
	}
	// 预检：仅白名单 origin 响应 CORS + PNA 头
	if r.Method == http.MethodOptions {
		if allowed == "" {
			http.Error(w, "forbidden origin", http.StatusForbidden)
		} else {
			w.Header().Set("Access-Control-Allow-Origin", allowed)
			w.Header().Set("Access-Control-Allow-Private-Network", "true")
			w.Header().Set("Access-Control-Allow-Headers", "x-aic-code, content-type")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.WriteHeader(http.StatusNoContent)
		}
		x.Stop()
		return nil
	}
	if allowed != "" {
		w.Header().Set("Access-Control-Allow-Origin", allowed)
		w.Header().Set("Access-Control-Allow-Private-Network", "true")
	}
	// 页面路径（/settings 等）放行——HTML 无敏感数据，数据读取走 /api/*（带 code 校验）；
	// /api/ping 无需凭证（探测用）。
	if strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/api/ping" && !l.checkCode(r) {
		http.Error(w, "invalid or expired local code", http.StatusUnauthorized)
		x.Stop()
		return nil
	}
	return nil
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

// dispatch 按端点分派（vigo Any 注册的入口）。
func (l *LocalAPI) dispatch(path string) func(*vigo.X) error {
	return func(x *vigo.X) error {
		w := x.ResponseWriter()
		switch path {
		case "/api/ping":
			if x.Request.Method != http.MethodGet {
				return methodNotAllowed(w)
			}
			_, _ = w.Write([]byte("pong"))
		case "/api/get_config":
			if x.Request.Method != http.MethodGet {
				return methodNotAllowed(w)
			}
			writeJSON(w, l.configView())
		case "/api/set_config":
			return l.handleSetConfig(w, x.Request)
		case "/api/bind":
			return l.handleBind(w, x.Request)
		case "/api/unbind":
			return l.handleUnbind(w, x.Request)
		case "/api/get_status":
			if x.Request.Method != http.MethodGet {
				return methodNotAllowed(w)
			}
			writeJSON(w, l.statusView())
		case "/api/get_log":
			if x.Request.Method != http.MethodGet {
				return methodNotAllowed(w)
			}
			// JSON 字符串（前端 bridge.get_log() 直接当文本用）
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(l.logContent())
		case "/api/start":
			return l.handleHostStart(w, x.Request)
		case "/api/stop":
			if x.Request.Method != http.MethodPost {
				return methodNotAllowed(w)
			}
			l.host.StopHost()
			writeJSON(w, map[string]bool{"ok": true})
		case "/api/open_url":
			return l.handleOpenURL(w, x.Request)
		case "/api/open_platform":
			return l.handleOpenPlatform(w, x.Request)
		case "/settings":
			return l.handleSettings(w, x.Request)
		}
		return nil
	}
}

func methodNotAllowed(w http.ResponseWriter) error {
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	return nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// handleBind 绑定设备：保存凭证并自动启动 host 会话。
// host 由启动参数决定（有效配置），页面只传 credential。
func (l *LocalAPI) handleBind(w http.ResponseWriter, r *http.Request) error {
	if r.Method != http.MethodPost {
		return methodNotAllowed(w)
	}
	var req struct {
		Credential string `json:"credential"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return nil
	}
	if strings.TrimSpace(req.Credential) == "" {
		http.Error(w, "credential is empty", http.StatusBadRequest)
		return nil
	}
	// 持久化凭证（基于文件配置，flag/env 覆盖不落盘）
	fileCfg, err := LoadConfig()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return nil
	}
	fileCfg.Key = strings.TrimSpace(req.Credential)
	if err := SaveConfig(fileCfg); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return nil
	}
	// 内存态同步：启动覆盖保留 + 新凭证
	l.mu.Lock()
	oldKey := l.cfg.Key
	l.cfg.Key = strings.TrimSpace(req.Credential)
	startCfg := l.cfg
	l.mu.Unlock()
	// 已运行时的 bind 语义（2026-08-06 修复）：desktop 启动时 autoConnect 已
	// StartHost，重复 bind 会误报 host already running 且阻断后续 set_config。
	// 凭证未变 → 保持运行（幂等）；凭证变了（换绑）→ 重启 host 用新凭证重连。
	if l.host.Running() {
		if oldKey != startCfg.Key {
			l.host.StopHost()
			if err := l.host.StartHost(startCfg); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return nil
			}
		}
	} else if err := l.host.StartHost(startCfg); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return nil
	}
	writeJSON(w, map[string]any{"ok": true, "host": fileCfg.Host})
	return nil
}

// handleOpenURL 用系统默认浏览器打开外链（desktop 外链委托，2026-08-06）：
// 平台窗口是 WKWebView（mac 未实现 createWebView，target=_blank 外链点击无反应），
// 前端 local_handler.js 拦截外链点击经此端点交给系统浏览器（自带多标签）。
// 仅接受 http/https（防本地命令注入），origin 校验在前端完成。
func (l *LocalAPI) handleOpenURL(w http.ResponseWriter, r *http.Request) error {
	if r.Method != http.MethodPost {
		return methodNotAllowed(w)
	}
	var req struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return nil
	}
	u, err := url.Parse(strings.TrimSpace(req.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		http.Error(w, "invalid url (http/https only)", http.StatusBadRequest)
		return nil
	}
	if err := OpenExternal(u.String()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return nil
	}
	writeJSON(w, map[string]bool{"ok": true})
	return nil
}

// handleOpenPlatform 应用内打开平台 URL（desktop 窗口跳转；cli 降级系统浏览器）。
func (l *LocalAPI) handleOpenPlatform(w http.ResponseWriter, r *http.Request) error {
	if r.Method != http.MethodPost {
		return methodNotAllowed(w)
	}
	var req struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return nil
	}
	u, err := url.Parse(strings.TrimSpace(req.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		http.Error(w, "invalid url (http/https only)", http.StatusBadRequest)
		return nil
	}
	if err := l.host.OpenPlatformURL(u.String()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return nil
	}
	writeJSON(w, map[string]bool{"ok": true})
	return nil
}

// handleSettings serve 本机设置页（desktop 主窗口 / cli 浏览器访问，GET）。
func (l *LocalAPI) handleSettings(w http.ResponseWriter, r *http.Request) error {
	if r.Method != http.MethodGet {
		return methodNotAllowed(w)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(settingsHTML(l.port, l.code)))
	return nil
}

func (l *LocalAPI) handleUnbind(w http.ResponseWriter, r *http.Request) error {
	if r.Method != http.MethodPost {
		return methodNotAllowed(w)
	}
	l.host.StopHost()
	fileCfg, err := LoadConfig()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return nil
	}
	fileCfg.Key = ""
	if err := SaveConfig(fileCfg); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return nil
	}
	l.mu.Lock()
	l.cfg.Key = ""
	l.mu.Unlock()
	writeJSON(w, map[string]bool{"ok": true})
	return nil
}

// handleHostStart 启动 host 会话（body 可携带临时覆盖，不持久化）。
func (l *LocalAPI) handleHostStart(w http.ResponseWriter, r *http.Request) error {
	if r.Method != http.MethodPost {
		return methodNotAllowed(w)
	}
	var req struct {
		Host       string `json:"host"`
		Credential string `json:"credential"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return nil
	}
	// 临时覆盖启动配置（不持久化）
	l.mu.Lock()
	cfg := l.cfg
	l.mu.Unlock()
	if strings.TrimSpace(req.Host) != "" {
		cfg.Host = strings.TrimSpace(req.Host)
	}
	if strings.TrimSpace(req.Credential) != "" {
		cfg.Key = strings.TrimSpace(req.Credential)
	}
	if err := l.host.StartHost(cfg); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return nil
	}
	writeJSON(w, map[string]bool{"ok": true})
	return nil
}

// loadEffective 返回当前有效配置（启动解析值 + 页面写操作同步，不持久化）。
func (l *LocalAPI) loadEffective() Config {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cfg
}

// configView 是 get_config 的返回视图（含 key——设置窗口需显示当前凭证；
// 本地 API 受 code 校验保护）。
type configView struct {
	Host        string `json:"host"`
	Key         string `json:"key"`
	WorkDir     string `json:"work_dir"`
	ExecTimeout string `json:"exec_timeout"`
}

func (l *LocalAPI) configView() configView {
	cfg := l.loadEffective()
	return configView{Host: cfg.Host, Key: cfg.Key, WorkDir: cfg.WorkDir, ExecTimeout: cfg.ExecTimeout}
}

func boundHostID(credential string) string {
	parts := strings.Split(strings.TrimSpace(credential), ".")
	if len(parts) == 4 {
		return parts[0]
	}
	return ""
}

// statusView 是 get_status 的返回：运行状态 + 基本信息（hostname/host_id/version）。
type statusView struct {
	Running  bool   `json:"running"`
	HostID   string `json:"host_id"`
	Hostname string `json:"hostname"`
	Version  string `json:"version"`
}

func (l *LocalAPI) statusView() statusView {
	hostname, _ := os.Hostname()
	return statusView{
		Running:  l.host.Running(),
		HostID:   boundHostID(l.loadEffective().Key),
		Hostname: hostname,
		Version:  l.version,
	}
}

// logContent 返回 get_log 内容（尾部最多缓冲行数）。
func (l *LocalAPI) logContent() string {
	if l.log == nil {
		return ""
	}
	return l.log.Content()
}

// HostsURL 由平台地址推导设备管理页入口 {host}/hosts（host 可带产品壳路径前缀，
// 如 http://127.0.0.1:4000/rses/aiv → http://127.0.0.1:4000/rses/aiv/hosts）。
// local_code 由调用方拼接（?local_code={port}.{code}）。
func HostsURL(hostURL string) string {
	h := strings.TrimSpace(hostURL)
	if h == "" {
		h = DefaultHost
	}
	if !strings.Contains(h, "://") {
		h = "https://" + h
	}
	u, err := url.Parse(h)
	if err != nil || u.Host == "" {
		return h
	}
	p := strings.TrimSuffix(u.Path, "/")
	if !strings.HasSuffix(p, "/hosts") {
		p += "/hosts"
	}
	u.Path = p
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}
func (l *LocalAPI) handleSetConfig(w http.ResponseWriter, r *http.Request) error {
	if r.Method != http.MethodPost {
		return methodNotAllowed(w)
	}
	var req struct {
		Host        string `json:"host"`
		WorkDir     string `json:"work_dir"`
		ExecTimeout string `json:"exec_timeout"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return nil
	}
	if s := strings.TrimSpace(req.ExecTimeout); s != "" {
		if _, err := time.ParseDuration(s); err != nil {
			http.Error(w, "invalid exec_timeout: "+err.Error(), http.StatusBadRequest)
			return nil
		}
	}
	// 持久化运行参数（基于文件配置）
	fileCfg, err := LoadConfig()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return nil
	}
	if strings.TrimSpace(req.Host) != "" {
		fileCfg.Host = strings.TrimSpace(req.Host)
	}
	// work_dir：~ 展开 + 归一绝对路径 + 有效性校验，落盘即真实路径
	//（Go exec 不做 shell 展开，配置页填 ~/test 会因路径不存在导致所有 exec 失败）
	wd := strings.TrimSpace(req.WorkDir)
	if wd != "" {
		wd = expandHome(wd)
		abs, err := filepath.Abs(wd)
		if err != nil {
			http.Error(w, "invalid work_dir: "+err.Error(), http.StatusBadRequest)
			return nil
		}
		st, err := os.Stat(abs)
		if err != nil || !st.IsDir() {
			http.Error(w, "invalid work_dir: not a directory: "+abs, http.StatusBadRequest)
			return nil
		}
		wd = abs
	}
	fileCfg.WorkDir = wd
	fileCfg.ExecTimeout = strings.TrimSpace(req.ExecTimeout)
	if err := SaveConfig(fileCfg); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return nil
	}
	l.mu.Lock()
	hostChanged := l.cfg.Host != fileCfg.Host
	workDirChanged := l.cfg.WorkDir != fileCfg.WorkDir
	execTimeoutChanged := l.cfg.ExecTimeout != fileCfg.ExecTimeout
	l.cfg.Host = fileCfg.Host
	l.cfg.WorkDir = fileCfg.WorkDir
	l.cfg.ExecTimeout = fileCfg.ExecTimeout
	cfg := l.cfg
	l.mu.Unlock()
	// 运行参数变更（host/work_dir/exec_timeout）：应用新配置——保留会话与
	// bg 任务，仅更新参数；NATS 地址变化时重连（Client.Reconfigure）
	if l.host.Running() && (hostChanged || workDirChanged || execTimeoutChanged) {
		if err := l.host.ApplyConfig(cfg); err != nil {
			logv.Warn().Msgf("apply config failed: %v", err)
		}
	}
	writeJSON(w, map[string]bool{"ok": true})
	return nil
}

// expandHome 展开 work_dir 的 ~ 前缀（~ 或 ~/xxx → 用户主目录）。
// 配置保存时调用：落盘即为真实绝对路径，运行时不再需要 shell 展开语义。
func expandHome(p string) string {
	if p == "~" {
		if h, err := os.UserHomeDir(); err == nil {
			return h
		}
		return p
	}
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, strings.TrimPrefix(p, "~/"))
		}
	}
	return p
}
