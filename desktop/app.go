package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/veypi/aic-pod/sdk/host"
	"github.com/wailsapp/wails/v3/pkg/application"
)

// App 是桌面壳的 service：配置管理 + 进程内 host 会话（同一进程，无子进程）。
type App struct {
	mu     sync.Mutex
	client *host.Client // nil = 未运行
	logs   []string     // 环形日志缓冲（最近 maxLogs 条），供前端拉取
	local  *LocalAPI    // 本地 HTTP 服务（local_code 通道）
}

const (
	maxLogs    = 500
	configFile = "config.json"
)

// ---- 配置 ----

// AppConfig 桌面配置。
type AppConfig struct {
	Host       string `json:"host"`
	Credential string `json:"credential"`
}

func defaultConfig() AppConfig {
	return AppConfig{Host: host.DefaultHost}
}

func (a *App) configPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "aic-desktop", configFile), nil
}

// GetConfig 读取配置（缺省返回默认值）。
func (a *App) GetConfig() (AppConfig, error) {
	p, err := a.configPath()
	if err != nil {
		return defaultConfig(), err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return defaultConfig(), nil // 未配置过
	}
	var cfg AppConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return defaultConfig(), err
	}
	if cfg.Host == "" {
		cfg.Host = host.DefaultHost
	}
	return cfg, nil
}

// SaveConfig 持久化配置。
func (a *App) SaveConfig(cfg AppConfig) error {
	p, err := a.configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(cfg, "", "  ")
	return os.WriteFile(p, data, 0o600)
}

// ---- host 会话（进程内） ----

// HostStatus 运行时状态。
type HostStatus struct {
	Running bool   `json:"running"`
	Log     string `json:"log"`
}

func (a *App) emitLog(line string) {
	a.mu.Lock()
	a.logs = append(a.logs, line)
	if len(a.logs) > maxLogs {
		a.logs = a.logs[len(a.logs)-maxLogs:]
	}
	a.mu.Unlock()
	// 测试/非 GUI 环境 application.Get() 为 nil，事件推送可跳过
	if app := application.Get(); app != nil {
		app.Event.Emit("host-log", line)
	}
}

func (a *App) status() HostStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	return HostStatus{Running: a.client != nil}
}

// StartHost 在应用进程内启动 host 会话（等价于 cli 的 Connect）。
func (a *App) StartHost(hostURL, credential string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.client != nil {
		return fmt.Errorf("host already running")
	}
	credential = strings.TrimSpace(credential)
	if credential == "" {
		return fmt.Errorf("credential is empty — save host and credential first")
	}
	if strings.TrimSpace(hostURL) == "" {
		hostURL = host.DefaultHost
	}
	c := host.New(host.Options{
		Credential: credential,
		NATSURL:    host.ResolveNATSURL(hostURL, ""),
		Version:    "0.5.1",
		OnLog: func(format string, args ...any) {
			a.emitLog(fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...)))
		},
	})
	if err := c.Connect(); err != nil {
		return err
	}
	a.client = c
	return nil
}

// StopHost 停止 host 会话。
func (a *App) StopHost() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.client != nil {
		a.client.Close()
		a.client = nil
	}
}

// HostStatusQuery 返回当前运行状态。
func (a *App) HostStatusQuery() HostStatus {
	return a.status()
}

// HostLog 返回日志缓冲（尾部最多 200 行）。
func (a *App) HostLog() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.logs) == 0 {
		return ""
	}
	start := 0
	if len(a.logs) > 200 {
		start = len(a.logs) - 200
	}
	return strings.Join(a.logs[start:], "\n")
}

// OpenPlatform 打开/聚焦平台窗口（远程 host 页面）。
func (a *App) OpenPlatform(hostURL string) error {
	if strings.TrimSpace(hostURL) == "" {
		cfg, err := a.GetConfig()
		if err != nil || cfg.Host == "" {
			hostURL = host.DefaultHost
		} else {
			hostURL = cfg.Host
		}
	}
	app := application.Get()
	// 拼 local_code 参数：平台页面据此建立本地通道（aic env.js 存 localStorage）
	if a.local != nil {
		sep := "?"
		if strings.Contains(hostURL, "?") {
			sep = "&"
		}
		hostURL = hostURL + sep + "local_code=" + url.QueryEscape(a.local.LocalCodeParam())
	}
	if w, ok := app.Window.Get("platform"); ok {
		w.SetURL(hostURL)
		w.Show()
		w.Focus()
		return nil
	}
	w := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:   "platform",
		Title:  "AIC Platform",
		URL:    hostURL,
		Width:  1280,
		Height: 800,
	})
	if w == nil {
		return fmt.Errorf("create platform window failed")
	}
	return nil
}
