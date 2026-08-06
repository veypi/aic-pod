// Copyright (C) 2025 veypi <i@veypi.com>
// Distributed under terms of the MIT license.

// Package pod 是 AIC 本地客户端（aic-pod）的根包：本地服务装配（Router +
// Start/Stop）。目录结构（对照 aic 服务端的分层）：
//
//	cfg/      配置中心：Options + Global（含 port/code 进程级隐私字段）、
//	          Version/DeviceType 二进制身份、日志文件写入
//	api/      本地管理端点（/api/*，自带 security 中间件与统一 JSON 响应）
//	libs/     客户端核心：host（NATS 会话运行时）、proto（协议信封签名）、
//	          vcore（虚拟指令）、exec_procs（进程托管）、utils（纯工具）
//	ui/       静态资源（settings.html 本机设置页）
//	cli/      命令行版本（aic）
//	desktop/  桌面版本（Wails v3 壳，独立 go module）
//	browser/  Chrome 扩展（纯 JS，MV3）
package pod

import (
	"context"
	_ "embed"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/veypi/aic-pod/api"
	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/host"
	"github.com/veypi/vigo"
	"github.com/veypi/vigo/logv"
)

// Router 是本地服务根路由：/api/* 端点（鉴权/CORS 由 api 包 security 承担）+
// /settings 本机设置页（公开页面，数据读取走 /api/*）。
var Router = vigo.NewRouter()

func init() {
	Router.Extend("/api", api.Router)
	Router.Get("/settings", "Settings Page", Settings)
}

var (
	mu  sync.Mutex
	srv *vigo.Application
)

// Start 监听 127.0.0.1 随机端口并启动服务；已绑定设备自动连接 host。
func Start() error {
	cfg.Global.Normalize()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	cfg.Global.SetPort(ln.Addr().(*net.TCPAddr).Port)

	s, err := vigo.NewServer(vigo.WithHost("127.0.0.1"), vigo.WithPort(0), vigo.WithListener(ln))
	if err != nil {
		ln.Close()
		return err
	}
	s.SetRouter(Router)
	mu.Lock()
	srv = s
	mu.Unlock()
	go func() { _ = s.Run() }()
	logv.WithNoCaller.Info().Msgf("local api listening on 127.0.0.1:%d (local_code=%s)", cfg.Global.Port(), LocalCodeParam())
	// 已绑定 → 自动连接 host（失败仅记日志，不阻断本地服务）
	if cfg.Global.Key != "" {
		if err := host.Start(*cfg.Global); err != nil {
			logv.Warn().Msgf("auto start host failed: %v", err)
		}
	}
	return nil
}

// Stop 关闭服务并停止 host 会话（应用退出时调用）。
func Stop() {
	mu.Lock()
	s := srv
	srv = nil
	mu.Unlock()
	host.Stop()
	if s != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	}
}

// LocalCodeParam 返回页面 URL 参数值：{port}.{code}。
func LocalCodeParam() string {
	return fmt.Sprintf("%d.%s", cfg.Global.Port(), cfg.Global.Code())
}

// ---- /settings 本机设置页（静态资源） ----

//go:embed ui/settings.html
var settingsHTML []byte

// Settings serve 本机设置页（ui/settings.html 静态资源，code 经 URL query
// 传入页面 JS）。数据不落平台——平台不可达时仍可配置 host/key（改错平台地址的
// 唯一兜底入口）；open_platform 走 Go 侧：desktop 应用内跳转，cli 降级系统浏览器。
// 根路由无 After 中间件——自行写响应。
func Settings(x *vigo.X) error {
	w := x.ResponseWriter()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, err := w.Write(settingsHTML)
	return err
}
