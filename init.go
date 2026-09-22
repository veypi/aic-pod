// Copyright (C) 2025 veypi <i@veypi.com>
// Distributed under terms of the MIT license.

// Package pod 是 AIC 本地客户端（aic-pod）的根包：host 会话装配（Start/Stop）。
// 目录结构（对照 aic 服务端的分层）：
//
//	cfg/       配置中心：Options + Global、Version/DeviceType 二进制身份、日志文件写入
//	settings/  本机设置面（`aic config get|set` 的读/写模型，原 api 包逻辑）
//	libs/      客户端核心：host（NATS 会话运行时）、proto（协议信封签名）、
//	          vcore（虚拟指令）、exec_procs（进程托管）、utils（纯工具）
//	cli/       命令行版本（aic）：连接运行 / config / bind / unbind
//	desktop/   Electron 壳（main.js + preload.js）：Chromium 窗口 + Go 后端子进程
//
// 本地不监听任何端口（2026-09-22 去本地管理 API / code / 端口文件握手）：
// 设置面 = config.yaml（Go 侧 flags 原子写），Electron 设置窗口经 IPC spawn
// `aic config|bind|unbind` 子命令读写；变更生效 = 重启后端子进程。
package pod

import (
	"strings"
	"time"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/host"
	"github.com/veypi/vigo/logv"
)

// Start 启动 host 会话：已绑定（key 非空）自动连接平台，未绑定仅提示。
func Start() error {
	cfg.Global.Normalize()
	logv.WithNoCaller.Info().Msgf("working on: %s", cfg.Global.WorkDir)
	// 已绑定 → 自动连接 host（失败按指数退避后台重试，不阻断启动）
	if cfg.Global.Key != "" {
		if err := host.Start(*cfg.Global); err != nil {
			logv.Warn().Msgf("auto start host failed: %v (retrying with backoff)", err)
			noteStartFailureFn(*cfg.Global, err, true)
			go retryHostStart(cfg.Global)
		}
	}
	return nil
}

// Stop 停止 host 会话（应用退出时调用）。
func Stop() {
	host.Stop()
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

// noteStartFailureFn 可注入（测试替身），生产实现为 host.NoteStartFailure：
// 退避重试期间把「未连接 + 原因（重试中）」落盘，桌面据此显示真实状态
// （2026-09-23「重连假成功」修复）。
var noteStartFailureFn = host.NoteStartFailure

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
				noteStartFailureFn(*o, err, false)
				return
			}
			logv.Warn().Msgf("auto start host failed: %v (retry in %v)", err, delay)
			noteStartFailureFn(*o, err, true)
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
