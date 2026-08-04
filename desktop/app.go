package main

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/veypi/aic-pod/sdk/host"
	"github.com/veypi/vigo/logv"
	"github.com/wailsapp/wails/v3/pkg/application"
)

// App 是桌面壳：host 会话生命周期（委托 sdk/host.Runner）+ 平台窗口。
// 配置读写与本地管理 API 全部在 sdk/host（cli/desktop 共享同一份 config.yaml
// 与同一套 local_code 通道）；desktop 只加 Wails 壳 + 首页窗口连接。
type App struct {
	runner *host.Runner  // host 生命周期（"desktop" 类型）
	local  *host.LocalAPI // 本地 HTTP 服务（local_code 通道）
	cfg    host.Config   // 启动时解析的有效配置（flag/env/文件，与 cli 同一套解析）
}

// NewApp 创建桌面壳。cfg 为 flags 解析后的有效配置。
func NewApp(cfg host.Config) *App {
	return &App{runner: host.NewRunner("desktop", version), cfg: cfg}
}

// config 返回启动时解析的有效配置（flags 解析结果，含 flag/env 覆盖）。
func (a *App) config() host.Config {
	return a.cfg
}

// StartHost 启动 host 会话（LocalHost 接口；Runner 内部自保护已运行检查）。
func (a *App) StartHost(cfg host.Config) error {
	return a.runner.StartHost(cfg)
}

// StopHost 停止 host 会话（LocalHost 接口）。
func (a *App) StopHost() {
	a.runner.StopHost()
}

// Running 报告 host 会话是否在运行（LocalHost 接口）。
func (a *App) Running() bool {
	return a.runner.Running()
}

// OpenPlatform 打开/聚焦平台窗口。
// 未绑定（无凭证）→ 直达设备管理页 {host}/hosts 引导绑定；已绑定 → 打开平台首页。
func (a *App) OpenPlatform(hostURL string) error {
	if strings.TrimSpace(hostURL) == "" {
		hostURL = a.config().Host
	}
	if a.config().Key == "" {
		hostURL = host.HostsURL(hostURL)
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
	logv.Info().Msgf("opening platform: %s", hostURL)
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
