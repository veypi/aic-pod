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
package pod

import (
	"context"
	"embed"
	_ "embed"
	"fmt"
	"net"
	"os"
	"strings"
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
	// flag/env 可覆盖文件配置；监听前再次确保 code 非空。
	if err := cfg.Global.EnsureCode(); err != nil {
		return err
	}

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
	// Electron 通过端口文件握手；写入失败属于启动失败，不能留下无法连接的服务。
	if pf := os.Getenv("AIC_PORT_FILE"); pf != "" {
		if err := os.WriteFile(pf, []byte(fmt.Sprintf(`{"port":%d,"code":%q}`, cfg.Global.Port(), cfg.Global.Code)), 0o600); err != nil {
			ln.Close()
			cfg.Global.SetPort(0)
			return fmt.Errorf("write port file %s: %w", pf, err)
		}
	}
	mu.Lock()
	srv = s
	mu.Unlock()
	go func() { _ = s.Run() }()
	// code 不进日志（2026-09-22）：logv 是终端+文件双写，desktop 壳经 AIC_PORT_FILE
	// 握手拿 code，CLI 形态的带 code 链接由 cli 层只写 stderr。
	logv.WithNoCaller.Info().Msgf("local api listening on 127.0.0.1:%d", cfg.Global.Port())
	logv.WithNoCaller.Info().Msgf("working on: %s", cfg.Global.WorkDir)
	// 已绑定 → 自动连接 host（失败按指数退避后台重试，不阻断本地服务）
	if cfg.Global.Key != "" {
		if err := host.Start(*cfg.Global); err != nil {
			logv.Warn().Msgf("auto start host failed: %v (retrying with backoff)", err)
			go retryHostStart(cfg.Global)
		}
	}
	return nil
}

// ---- auto-start 退避重试 ----
//
// 初次连接失败多为暂时性错误（平台重启间隙、NATS 槽位未及时释放、网络未就绪），
// 应退避重试而非只尝试一次：host 是常驻服务，平台随时可能恢复。
// 初始 5s，指数翻倍，上限 5min，无限重试直到成功或遇到永久错误。

const (
	hostRetryInitial = 5 * time.Second
	hostRetryMax     = 5 * time.Minute
)

// hostStartFn 可注入（测试替身），生产实现为 host.Start。
var hostStartFn = host.Start

// retryHostStart 在后台按指数退避重试 host 启动。
func retryHostStart(o *cfg.Options) {
	retryHostStartWith(o, hostStartFn, time.Sleep, hostRetryInitial, hostRetryMax)
}

func retryHostStartWith(o *cfg.Options, start func(cfg.Options) error, sleep func(time.Duration), initial, max time.Duration) {
	delay := initial
	for {
		sleep(delay)
		if err := start(*o); err != nil {
			if isPermanentStartErr(err) {
				logv.Warn().Msgf("auto start host aborted: %v", err)
				return
			}
			logv.Warn().Msgf("auto start host failed: %v (retry in %v)", err, delay)
			delay = nextRetryDelay(delay, max)
			continue
		}
		logv.Info().Msg("auto start host connected after retry")
		return
	}
}

// isPermanentStartErr 判定无需重试的启动错误：凭证缺失/格式非法（key 解析在
// Client.Connect 内，格式错不会因重试而变好）、已被其他路径启动。
// 注意：Authorization Violation 不在此列——槽位未释放也会报该错（暂时性），
// 与凭证被吊销无法在初次连接阶段区分，只能退避重试。
func isPermanentStartErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "key is empty") ||
		strings.Contains(s, "invalid credential key") || // 含 "invalid credential key version"
		strings.Contains(s, "already running")
}

// nextRetryDelay 指数退避：翻倍，封顶 max。
func nextRetryDelay(delay, max time.Duration) time.Duration {
	delay *= 2
	if delay > max {
		return max
	}
	return delay
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
