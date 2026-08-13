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
//	desktop/  Electron 壳（main.js + preload.js）：Chromium 窗口 + Go 后端子进程（cli 二进制）
//	browser/  Chrome 扩展（纯 JS，MV3）
package pod

import (
	"context"
	"embed"
	_ "embed"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/veypi/aic-pod/api"
	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/host"
	"github.com/veypi/vhtml"
	"github.com/veypi/vigo"
	"github.com/veypi/vigo/logv"
)

// Router 是本地服务根路由：/api/* 端点（鉴权/CORS 由 api 包 security 承担）+
// /settings 本机设置页（公开页面，数据读取走 /api/*）。
var Router = vigo.NewRouter()

//go:embed ui
var uifs embed.FS

func init() {
	Router.Extend("/api", api.Router)
	Router.Extend("vhtml", vhtml.Router)
	_ = vhtml.WrapUI(Router, uifs)
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
	logv.WithNoCaller.Info().Msgf("local api listening on 127.0.0.1:%d (code=%s)", cfg.Global.Port(), cfg.Global.Code)
	logv.WithNoCaller.Info().Msgf("working on: %s", cfg.Global.WorkDir)
	// 端口上报（Electron 壳握手）：AIC_PORT_FILE 指定 JSON 文件路径时写入
	// {port, code}，Electron 主进程据此加载壳页面（cli 正常使用不受影响）。
	if pf := os.Getenv("AIC_PORT_FILE"); pf != "" {
		if err := os.WriteFile(pf, []byte(fmt.Sprintf(`{"port":%d,"code":%q}`, cfg.Global.Port(), cfg.Global.Code)), 0o600); err != nil {
			logv.Warn().Msgf("write port file %s: %v", pf, err)
		}
	}
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
