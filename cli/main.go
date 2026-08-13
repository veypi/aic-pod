// AIC CLI — 部署在 PC (Windows/macOS/Linux) 上的 host agent，通过 NATS 连接 AIC 平台。
//
// 主命令即运行（`aic`），无子指令；临时参数走 flag（AutoRegister），
// 永久生效用户直接改配置文件（UserConfigDir/aic/config.yaml，cli/desktop 共享，cfg 包）。
//
// 配置解析由 vigo/flags 承担（AutoRegister 自动注册 flag + env，只需配置结构体）：
//
//	flag：-host / -key / -work_dir / -exec_timeout
//	env ：HOST / KEY / WORK_DIR / EXEC_TIMEOUT
//
// 解析链：显式 flag > env > 配置文件（config.yaml）> 结构体 default tag
// 日志统一 vigo/logv（cli：console + 文件双写，get_log 读日志文件尾部）。
package main

import (
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"syscall"

	pod "github.com/veypi/aic-pod"
	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/vigo/flags"
	"github.com/veypi/vigo/logv"
)

func main() {
	// 客户端身份：Electron 壳以 env 指定 desktop（设备列表显示类型）；cli 默认不变
	if dt := os.Getenv("AIC_DEVICE_TYPE"); dt != "" {
		cfg.DeviceType = dt
	}
	// 日志：console（终端）+ 文件（cfg.LogPath，get_log 数据源）
	if lw, err := cfg.LogWriter(); err != nil {
		logv.Warn().Msgf("log file writer: %v", err)
		logv.SetLogger(logv.NewLogger(logv.ConsoleWriter()))
	} else {
		logv.SetLogger(logv.NewLogger(logv.ConsoleWriter(), lw))
	}

	// 配置：文件值填充 cfg.Global 后交给 AutoRegister（flag/env 覆盖）
	o, err := cfg.Load()
	if err != nil {
		logv.Warn().Msgf("load config: %v", err)
	}

	cmd := flags.New("aic", "AIC host agent (local client)")
	cmd.AutoRegister(o)

	// 主命令：连接运行（本地 API + host 会话）
	cmd.Command = func() error { return runCmd() }

	cmd.Parse()
	if err := cmd.Run(); err != nil {
		logv.Error().Msg(err.Error())
		os.Exit(1)
	}
}

// runCmd 启动本地管理 API（含已绑定时自动连接 host），打印带 code 的本地壳
// 引导链接（用户浏览器访问即绑定/管理本机），阻塞等待 SIGINT/SIGTERM。
func runCmd() error {
	if err := pod.Start(); err != nil {
		return err
	}
	defer pod.Stop()

	// 带 code 的引导链接：本地壳页面（header + iframe 平台页，与桌面同一体验）
	link := fmt.Sprintf("http://127.0.0.1:%d/?code=%s", cfg.Global.Port(), url.QueryEscape(cfg.Global.Code))
	logv.Info().Msgf("aic %s (host=%s)", cfg.Version, cfg.Global.Host)
	logv.Info().Msgf("management page: %s", link)
	logv.Info().Msgf("local api: http://127.0.0.1:%d", cfg.Global.Port())

	if cfg.Global.Key == "" {
		// 未绑定 → 提示去页面绑定（不退出）
		logv.Warn().Msg("no key — open the management page above to bind a device")
	}

	// 阻塞等待 SIGINT/SIGTERM
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	logv.Info().Msg("shutting down...")
	return nil
}
