package main

import (
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/veypi/aic-pod/sdk/host"
	"github.com/wailsapp/wails/v3/pkg/application"
)

// App 是桌面壳的 service：host 会话生命周期 + 日志缓冲 + 平台窗口。
// 配置读写不在这里——统一走 sdk/host 的 Config（UserConfigDir/aic/config.json，
// 与 cli 共享同一份）。
type App struct {
	mu     sync.Mutex   // host 会话锁
	logMu  sync.Mutex   // 日志缓冲锁（独立于 mu：StartHost 持 mu 时 OnLog 回调仍可写日志）
	client *host.Client // nil = 未运行
	logs   []string     // 环形日志缓冲（最近 maxLogs 条），供前端拉取
	local  *LocalAPI    // 本地 HTTP 服务（local_code 通道）
}

const maxLogs = 500

// AppConfig 是 host.Config 的别名（Wails 绑定与测试沿用旧名）。
type AppConfig = host.Config

// ---- 配置（薄封装：有效配置 = 配置文件 + AIC_* 环境变量覆盖） ----

// config 返回本次运行的有效配置（env 覆盖不持久化）。
func (a *App) config() host.Config {
	cfg, err := host.Load()
	if err != nil {
		a.emitLog(fmt.Sprintf("load config: %v", err))
	}
	return cfg
}

// GetConfig 返回有效配置（localapi 展示用）。
func (a *App) GetConfig() (host.Config, error) {
	return a.config(), nil
}

// SaveConfig 持久化配置到共享配置文件。
func (a *App) SaveConfig(cfg host.Config) error {
	return host.SaveConfig(cfg)
}

// ---- host 会话（进程内） ----

// HostStatus 运行时状态。
type HostStatus struct {
	Running bool   `json:"running"`
	Log     string `json:"log"`
}

func (a *App) emitLog(line string) {
	// 同步输出到 stdout（桌面壳无 UI，排查依赖终端日志）
	fmt.Println(line)
	a.logMu.Lock()
	a.logs = append(a.logs, line)
	if len(a.logs) > maxLogs {
		a.logs = a.logs[len(a.logs)-maxLogs:]
	}
	a.logMu.Unlock()
	// 测试/非 GUI 环境 application.Get() 为 nil，事件推送可跳过
	if app := application.Get(); app != nil {
		app.Event.Emit("host-log", line)
	}
}

// StartHost 在应用进程内启动 host 会话（与 cli `aic run` 同一入口）。
func (a *App) StartHost(cfg host.Config) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.client != nil {
		return fmt.Errorf("host already running")
	}
	if strings.TrimSpace(cfg.Credential) == "" {
		return fmt.Errorf("credential is empty — bind a device first")
	}
	opts, err := cfg.Options("desktop", version, func(format string, args ...any) {
		a.emitLog(fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...)))
	})
	if err != nil {
		return err
	}
	c, err := host.Connect(opts)
	if err != nil {
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
	a.mu.Lock()
	defer a.mu.Unlock()
	return HostStatus{Running: a.client != nil}
}

// HostLog 返回日志缓冲（尾部最多 200 行）。
func (a *App) HostLog() string {
	a.logMu.Lock()
	defer a.logMu.Unlock()
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
		hostURL = a.config().Host
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
