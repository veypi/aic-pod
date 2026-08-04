package main

import (
	"embed"
	"log"

	"github.com/wailsapp/wails/v3/pkg/application"
)

//go:embed all:frontend
var assets embed.FS

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

	app := application.New(application.Options{
		Name:        "AIC Desktop",
		Description: "AIC Desktop — 页面访问平台，进程内托管 host",
		Services: []application.Service{
			application.NewService(svc),
		},
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(assets),
		},
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: true,
		},
	})

	w := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:     "AIC Desktop",
		Width:     960,
		Height:    680,
		MinWidth:  720,
		MinHeight: 480,
		URL:       "/",
	})
	if w == nil {
		log.Fatal("create main window failed")
	}

	// 应用退出时停止 host 会话（防止 NATS 连接残留）并关闭本地服务
	defer svc.StopHost()
	defer local.Stop()

	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}
