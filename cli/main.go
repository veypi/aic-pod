// AIC CLI — 部署在 PC (Windows/macOS/Linux) 上的 host agent，通过 NATS 连接 AIC 平台。
//
// 主命令即运行（`aic`）；唯一子指令 `wake`：唤醒桌宠/pet 页录音（仅 desktop 形态，
// 经 Electron 本地指令通道转发 pet:cmd 事件，效果等同 pet 页左键单击）。
// 临时参数走 flag（AutoRegister），永久生效用户直接改配置文件
// （UserConfigDir/aic/config.yaml，cli/desktop 共享，cfg 包）。
//
// 配置解析由 vigo/flags 承担（AutoRegister 自动注册 flag + env，只需配置结构体）：
//
//	flag：-host / -key / -work_dir / -exec_timeout / -home_path
//	env ：HOST / KEY / WORK_DIR / EXEC_TIMEOUT / HOME_PATH
//
// 解析链：显式 flag > env > 配置文件（config.yaml）> 结构体 default tag
// 日志统一 vigo/logv（cli：console + 文件双写，get_log 读日志文件尾部）。
package main

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	pod "github.com/veypi/aic-pod"
	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/uiscript"
	"github.com/veypi/vigo/flags"
	"github.com/veypi/vigo/logv"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == uiscript.WorkerArg {
		os.Exit(uiscript.Main())
	}
	// 子指令：wake = 唤醒桌宠录音（仅 desktop 形态；socket 由 Electron 主进程创建）
	if len(os.Args) > 1 && os.Args[1] == "wake" {
		if err := pod.Wake(); err != nil {
			fmt.Fprintln(os.Stderr, "wake:", err)
			os.Exit(1)
		}
		return
	}

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

	cmd := flags.New("aic", "AIC host agent (local client)")
	cmd.AutoRegister(cfg.Global)
	if p, err := cfg.Path(); err == nil {
		cmd.ConfigFile(p)
	}

	// 主命令：连接运行（本地 API + host 会话）
	cmd.Command = func() error { return runCmd() }

	if err := cmd.Parse(); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		logv.Error().Msg(err.Error())
		os.Exit(2)
	}
	cfg.Global.ApplyConfigIssues(cmd.ConfigIssues())
	for _, issue := range cmd.ConfigIssues() {
		logv.Warn().Msgf("configuration issue (%s, %s): %s", issue.Source, issue.Field, issue.Reason)
	}
	if err := cfg.Global.ValidateAuth(); err != nil {
		logv.Warn().Msg("device tools disabled: invalid authorization configuration; repair local settings")
	}
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

	// desktop 形态（Electron 壳 spawn）：父进程消失即自行退出——壳被强杀/崩溃时
	// 不会走 will-quit 回收，子进程若无自检会滞留为孤儿（2026-09 实测事故）。
	if cfg.DeviceType == "desktop" {
		go exitWhenParentGone()
	}

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

// exitWhenParentGone 轮询父进程是否消失（进程被 launchd 收养后 ppid 变化即视为
// 消失），是则停止服务并退出。仅 desktop 形态启用：cli 常驻用法不受影响；
// Windows 无 ppid 收养语义，检查不触发（保持原行为）。
func exitWhenParentGone() {
	ppid := os.Getppid()
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for range t.C {
		if os.Getppid() == ppid {
			continue
		}
		logv.Warn().Msg("parent process gone — exiting")
		pod.Stop()
		os.Exit(0)
	}
}
