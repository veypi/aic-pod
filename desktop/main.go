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

// main 与 cli 同一套 vigo/flags 解析（-h / -host / -key / -work_dir / -exec_timeout / -code，
// env: HOST / KEY / WORK_DIR / EXEC_TIMEOUT / CODE），主命令启动 Wails 壳。
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

// runApp 启动 Wails 壳：本地服务（vigo 壳页面 + /api）+ frameless 壳窗口。
func runApp() error {
	svc := NewApp()

	// 窗口控制注入（壳页面 /api/window_* → wails window API；cli 无窗口不注入）
	api.WindowControl = windowControl

	if err := pod.Start(); err != nil {
		return err
	}

	app := application.New(application.Options{
		Name:        "AIC Desktop",
		Description: "AIC Desktop — 本地壳页面 + 进程内托管 host",
		SingleInstance: &application.SingleInstanceOptions{
			UniqueID: "ai.ivec.desktop",
			ExitCode: 0,
			OnSecondInstanceLaunch: func(_ application.SecondInstanceData) {
				// 已运行：聚焦现有壳窗口（本地服务端口唯一，避免多实例端口失配）
				svc.FocusPlatform()
			},
		},
		// 常驻托盘：关闭窗口不退出应用
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: false,
		},
	})

	// 壳窗口（本地页面：header + iframe 平台页；无系统菜单/无系统按钮）
	if err := svc.OpenShell(); err != nil {
		return err
	}

	// 常驻托盘：关闭按钮 → 隐藏窗口（进程 + host 会话 + 本地服务继续运行）
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
	quit := menu.Add("退出")
	open.OnClick(func(_ *application.Context) {
		svc.FocusPlatform()
	})
	quit.OnClick(func(_ *application.Context) {
		app.Quit()
	})
	tray.SetMenu(menu)
	tray.OnClick(func() {
		svc.FocusPlatform()
	})

	// 应用退出时停止 host 会话（防止 NATS 连接残留）并关闭本地服务
	defer pod.Stop()

	return app.Run()
}
