package main

import (
	"testing"

	"github.com/veypi/aic-pod/sdk/host"
)

// 桌面壳的 host 生命周期委托 sdk/host.Runner；HostsURL 已迁移 sdk/host
//（测试见 sdk/host/localapi_test.go TestHostsURL）。
func TestNewApp(t *testing.T) {
	app := NewApp(host.Config{})
	if app == nil || app.runner == nil {
		t.Fatal("NewApp should initialize runner")
	}
	if app.Running() {
		t.Fatal("new app should not be running")
	}
}
