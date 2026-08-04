package main

import (
	_ "embed"
	"os"

	"github.com/veypi/aic-pod/sdk/host"
	"github.com/veypi/vigo/flags"
	"github.com/veypi/vigo/logv"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

// 托盘图标（win 右下角 / mac 菜单栏，AIC logo 128px）。
//
//go:embed trayicon.png
var trayIcon []byte

// version 与 cli 同一来源：Makefile desktop target 经 -X main.version=$(VERSION)
// 注入 git 版本（如 v0.5.1），desktop 与 cli 永远同一版本。
var version = "v0.5.2"

// ---- 日志（统一 vigo/logv；get_log 经 RingBuffer 挂入 logv） ----

// logRing 是日志环形缓冲（本地 API get_log 的数据源）。
var logRing = host.NewRingBuffer(500)

// main 与 cli 同一套 vigo/flags 解析（-h / -host / -key / -work_dir / -exec_timeout，
// env: HOST / KEY / WORK_DIR / EXEC_TIMEOUT），主命令启动 Wails 壳。
func main() {
	// 日志：console（终端）+ ring（get_log）——与 cli 同一套
	logv.SetLogger(logv.NewLogger(logv.ConsoleWriter(), logRing))

	// 配置结构体：文件默认值填充后交给 AutoRegister（flag/env 覆盖）
	cfg, err := host.LoadConfig()
	if err != nil {
		logv.Warn().Msgf("load config: %v", err)
	}

	cmd := flags.New("aic-desktop", "AIC Desktop — Wails shell (host agent)")
	cmd.AutoRegister(&cfg)
	// 主命令：启动 Wails 壳（窗口 + 进程内 host 会话）
	cmd.Command = func() error { return runApp(&cfg) }

	cmd.Parse()
	if err := cmd.Run(); err != nil {
		logv.Error().Msg(err.Error())
		os.Exit(1)
	}
}

// runApp 启动 Wails 壳：本地管理 API + 自动连接 host + 平台窗口。
func runApp(cfg *host.Config) error {
	svc := NewApp(*cfg)

	local, err := host.NewLocalAPI(svc, version, logRing, *cfg)
	if err != nil {
		return err
	}
	if err := local.Start(); err != nil {
		return err
	}
	svc.local = local

	// 已配置的设备：后台自动连接 host（失败记录日志，不阻塞窗口）
	if cfg.Key != "" {
		if err := svc.StartHost(*cfg); err != nil {
			logv.Warn().Msgf("auto start host failed: %v", err)
		}
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

	// 主窗口 = 平台页面（URL 自动携带 ?local_code={port}.{code}，
	// 本地配置统一走平台 /hosts 页，桌面壳不再提供本地管理页）
	if err := svc.OpenPlatform(""); err != nil {
		return err
	}

	// 常驻托盘：关闭按钮 → 隐藏窗口（进程 + host 会话 + LocalAPI 继续运行）
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
	defer svc.StopHost()
	defer local.Stop()

	return app.Run()
}
