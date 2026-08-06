// AIC CLI — 部署在 PC (Windows/macOS/Linux) 上的 host agent，通过 NATS 连接 AIC 平台。
//
// 主命令即运行（`aic`），无子指令；临时参数走 flag（AutoRegister），
// 永久生效用户直接改配置文件（UserConfigDir/aic/config.yaml，cli/desktop 共享）。
//
// 配置解析由 vigo/flags 承担（AutoRegister 自动注册 flag + env，只需配置结构体）：
//
//	flag：-host / -key / -work_dir / -exec_timeout
//	env ：HOST / KEY / WORK_DIR / EXEC_TIMEOUT
//
// 解析链：显式 flag > env > 配置文件（config.yaml）> 结构体 default tag
// 日志统一 vigo/logv（console + RingBuffer，本地 API get_log 经 RingBuffer 读取）。
package main

import (
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/veypi/aic-pod/sdk/host"
	"github.com/veypi/aic-pod/sdk/windpi"
	"github.com/veypi/vigo/flags"
	"github.com/veypi/vigo/logv"
)

var version = "v0.5.3"

// logRing 是日志环形缓冲（本地 API get_log 的数据源）。
var logRing = host.NewRingBuffer(500)

func main() {
	// Windows：进程级 DPI 感知——子进程（powershell/cmd/...）继承该状态，
	// 屏幕像素 API（GetSystemMetrics/CopyFromScreen）按物理分辨率返回（§windpi）。
	windpi.Enable()

	// 日志：console（终端）+ ring（get_log）
	logv.SetLogger(logv.NewLogger(logv.ConsoleWriter(), logRing))

	// 配置结构体：文件默认值填充后交给 AutoRegister（flag/env 覆盖）
	cfg, err := host.LoadConfig()
	if err != nil {
		logv.Warn().Msgf("load config: %v", err)
	}

	cmd := flags.New("aic", "AIC host agent (local client)")
	cmd.AutoRegister(&cfg)

	// 主命令：连接运行（LocalAPI + host 会话）
	cmd.Command = func() error { return runCmd(&cfg) }

	cmd.Parse()
	if err := cmd.Run(); err != nil {
		logv.Error().Msg(err.Error())
		os.Exit(1)
	}
}

// runCmd 启动本地管理 API + host 会话，打印带 local_code 的引导链接
// （用户浏览器访问即绑定/管理本机），阻塞等待 SIGINT/SIGTERM。
func runCmd(cfg *host.Config) error {
	runner := host.NewRunner("cli", version)
	api, err := host.NewLocalAPI(runner, version, logRing, *cfg)
	if err != nil {
		return err
	}
	if err := api.Start(); err != nil {
		return err
	}
	defer api.Stop()

	// 带 local_code 的引导链接：{host}/hosts?local_code={port}.{code}
	link := host.HostsURL(cfg.Host)
	sep := "?"
	if strings.Contains(link, "?") {
		sep = "&"
	}
	link += sep + "local_code=" + url.QueryEscape(api.LocalCodeParam())
	logv.Info().Msgf("aic %s (host=%s)", version, cfg.Host)
	logv.Info().Msgf("management page: %s", link)
	logv.Info().Msgf("local api: http://127.0.0.1:%d", api.Port())

	// 已绑定 → 自动连接；未绑定 → 提示去页面绑定（不退出）
	if strings.TrimSpace(cfg.Key) != "" {
		if err := runner.StartHost(*cfg); err != nil {
			logv.Warn().Msgf("auto start host failed: %v", err)
		}
	} else {
		logv.Warn().Msg("no key — open the management page above to bind a device")
	}

	// 阻塞等待 SIGINT/SIGTERM
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	logv.Info().Msg("shutting down...")
	runner.StopHost()
	return nil
}
