package main

import (
	_ "embed"
	"os"

	pod "github.com/veypi/aic-pod"
	"github.com/veypi/aic-pod/api"
	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/vigo/flags"
	"github.com/veypi/vigo/logv"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

// 托盘图标（win 右下角 / mac 菜单栏，AIC logo 128px）。
//
//go:embed trayicon.png
var trayIcon []byte

// main 与 cli 同一套 vigo/flags 解析（-h / -host / -key / -work_dir / -exec_timeout，
// env: HOST / KEY / WORK_DIR / EXEC_TIMEOUT），主命令启动 Wails 壳。
func main() {
	// 二进制身份：桌面壳（api.Init 据此创建 Runner）
	cfg.DeviceType = "desktop"

	// 日志：仅文件（cfg.LogPath，get_log 数据源；GUI 无终端）
	if lw, err := cfg.LogWriter(); err == nil {
		logv.SetLogger(logv.NewLogger(lw))
	}

	// 配置：文件值填充 cfg.Global 后交给 AutoRegister（flag/env 覆盖）
	o, err := cfg.Load()
	if err != nil {
		logv.Warn().Msgf("load config: %v", err)
	}

	cmd := flags.New("aic-desktop", "AIC Desktop — Wails shell (host agent)")
	cmd.AutoRegister(o)
	// 主命令：启动 Wails 壳（窗口 + 进程内 host 会话）
	cmd.Command = func() error { return runApp() }

	cmd.Parse()
	if err := cmd.Run(); err != nil {
		logv.Error().Msg(err.Error())
		os.Exit(1)
	}
}

// runApp 启动 Wails 壳：本地管理 API + 平台窗口。
func runApp() error {
	svc := NewApp()

	// open_platform 走应用内窗口跳转（默认系统浏览器仅 cli 用）
	api.OpenPlatformURL = svc.OpenPlatformURL
	if err := pod.Start(); err != nil {
		return err
	}

	app := application.New(application.Options{
		Name:        "AIC Desktop",
		Description: "AIC Desktop — 页面访问平台，进程内托管 host",
		SingleInstance: &application.SingleInstanceOptions{
			UniqueID: "ai.ivec.desktop",
			ExitCode: 0,
			OnSecondInstanceLaunch: func(_ application.SecondInstanceData) {
				// 已运行：聚焦现有平台窗口（local_code 端口唯一，避免多实例端口失配）
				if w, ok := application.Get().Window.Get("platform"); ok {
					w.Show()
					w.Focus()
				}
			},
		},
		// 常驻托盘：关闭窗口不退出应用（默认即 false，显式声明）
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: false,
		},
	})

	// 原生菜单：mac 菜单栏 / windows 窗口菜单栏（linux 无应用菜单支持，自动降级）。
	// 编辑菜单同时是 mac webview 内 Cmd+C/V/Z 生效的前提（原生角色经 responder chain）。
	app.Menu.SetApplicationMenu(buildAppMenu(svc))

	// 主窗口 = 平台页面（URL 自动携带 ?local_code={port}.{code}，
	// 本地配置统一走平台 /hosts 页，桌面壳不再提供本地管理页）
	if err := svc.OpenPlatform(""); err != nil {
		return err
	}

	// 常驻托盘：关闭按钮 → 隐藏窗口（进程 + host 会话 + 本地 API 继续运行）
	// RegisterHook 与 wails 官方 hide-window 示例一致（hook 先于默认关闭处理）
	win := svc.PlatformWindow()
	win.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		win.Hide()
		e.Cancel()
	})

	// mac Dock 图标点击 → 重新显示窗口（reopen 语义）
	app.Event.OnApplicationEvent(events.Mac.ApplicationShouldHandleReopen, func(_ *application.ApplicationEvent) {
		win.Show()
	})

	// 托盘图标：win 右下角 / mac 菜单栏；左键点击打开/聚焦窗口
	tray := app.SystemTray.New()
	tray.SetIcon(trayIcon)
	tray.SetTooltip("AIC Desktop")
	menu := application.NewMenu()
	open := menu.Add("打开")
	settings := menu.Add("设置")
	quit := menu.Add("退出")
	open.OnClick(func(_ *application.Context) {
		win.Show().Focus()
	})
	settings.OnClick(func(_ *application.Context) {
		svc.openSettings()
	})
	quit.OnClick(func(_ *application.Context) {
		app.Quit()
	})
	tray.SetMenu(menu)
	tray.OnClick(func() {
		win.Show().Focus()
	})

	// 应用退出时停止 host 会话（防止 NATS 连接残留）并关闭本地服务
	defer pod.Stop()

	return app.Run()
}
