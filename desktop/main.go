package main

import (
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// version 与 cli 同一来源：Makefile desktop target 经 -X main.version=$(VERSION)
// 注入 git 版本（如 v0.5.1），desktop 与 cli 永远同一版本。
var version = "v0.5.1"

func main() {
	svc := &App{}

	// HOST 环境变量：仅本次运行覆盖平台地址（不持久化，配置只由 /settings/local 管理）
	if h := strings.TrimSpace(os.Getenv("HOST")); h != "" {
		svc.hostOVR = h
	}

	local, err := newLocalAPI(svc)
	if err != nil {
		log.Fatal(err)
	}
	if err := local.Start(); err != nil {
		log.Fatal(err)
	}
	svc.local = local

	// 已配置的设备：后台自动连接 host（失败记录日志，不阻塞窗口）
	if cfg, err := svc.GetConfig(); err == nil && cfg.Credential != "" {
		hostURL := cfg.Host
		if svc.hostOVR != "" {
			hostURL = svc.hostOVR
		}
		if err := svc.StartHost(hostURL, cfg.Credential); err != nil {
			svc.emitLog(fmt.Sprintf("auto start host failed: %v", err))
		}
	}

	app := application.New(application.Options{
		Name:        "AIC Desktop",
		Description: "AIC Desktop — 页面访问平台，进程内托管 host",
		Services: []application.Service{
			application.NewService(svc),
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
