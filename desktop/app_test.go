package main

import (
	"testing"
)

// 桌面壳只管平台窗口；host 会话生命周期在 api 包（Runner）。
func TestNewApp(t *testing.T) {
	app := NewApp()
	if app == nil {
		t.Fatal("NewApp should return app")
	}
	if app.platform != nil {
		t.Fatal("new app should have no window")
	}
}

// 托盘图标必须被 go:embed 打入二进制。
func TestTrayIconEmbedded(t *testing.T) {
	if len(trayIcon) == 0 {
		t.Fatal("tray icon not embedded")
	}
}
