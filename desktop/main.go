package main

import (
	"fmt"
	"log"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// version 与 cli 同一来源：Makefile desktop target 经 -X main.version=$(VERSION)
// 注入 git 版本（如 v0.5.1），desktop 与 cli 永远同一版本。
var version = "v0.5.1"

func main() {
	svc := &App{}

	local, err := newLocalAPI(svc)
	if err != nil {
		log.Fatal(err)
	}
	if err := local.Start(); err != nil {
		log.Fatal(err)
	}
	svc.local = local

	// 已配置的设备：后台自动连接 host（失败记录日志，不阻塞窗口）。
	// 有效配置 = 配置文件 + AIC_* 环境变量覆盖（与 cli 同一解析链）。
	if cfg := svc.config(); cfg.Credential != "" {
		if err := svc.StartHost(cfg); err != nil {
			svc.emitLog(fmt.Sprintf("auto start host failed: %v", err))
		}
	}

	app := application.New(application.Options{
		Name:        "AIC Desktop",
		Description: "AIC Desktop — 页面访问平台，进程内托管 host",
		Services: []application.Service{
			application.NewService(svc),
		},
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
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: true,
		},
	})

	// 主窗口 = 平台页面（URL 自动携带 ?local_code={port}.{code}，
	// 本地配置统一走平台 /settings/local，桌面壳不再提供本地管理页）
	if err := svc.OpenPlatform(""); err != nil {
		log.Fatal(err)
	}

	// 应用退出时停止 host 会话（防止 NATS 连接残留）并关闭本地服务
	defer svc.StopHost()
	defer local.Stop()

	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}
