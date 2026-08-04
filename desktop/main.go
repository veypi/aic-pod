package main

import (
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/wailsapp/wails/v3/pkg/application"
)

func main() {
	svc := &App{}

	// HOST 环境变量：启动时注入平台地址（持久化，local 页面 getConfig 一致）
	if h := strings.TrimSpace(os.Getenv("HOST")); h != "" {
		cfg, _ := svc.GetConfig()
		cfg.Host = h
		_ = svc.SaveConfig(cfg)
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
		if err := svc.StartHost(cfg.Host, cfg.Credential); err != nil {
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
