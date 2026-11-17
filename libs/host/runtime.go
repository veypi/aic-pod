package host

import (
	"fmt"
	"strings"
	"sync"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/vigo/logv"
)

// host 会话生命周期（进程单例，cli/desktop 同一套）。
// 日志统一走 vigo/logv（不建自有日志系统），get_log 读 logv 落盘的日志文件。
var (
	rtMu     sync.Mutex
	rtClient *Client
)

type connectClientFunc func(Options) (*Client, error)

// Start 启动静态设备会话；原生 Session 客户端使用 StartOptions。
func Start(o cfg.Options) error {
	if strings.TrimSpace(o.Key) == "" {
		return fmt.Errorf("key is empty — bind a device first")
	}
	opts, err := optionsOf(o, cfg.DeviceType, cfg.Version, func(format string, args ...any) {
		logv.WithNoCaller.Info().Msgf(format, args...)
	})
	if err != nil {
		return err
	}
	return StartOptions(opts)
}

// StartSession 以 Desktop 原生会话配置启动 Host，并沿用本机工作区、权限与传输设置。
// config.yaml 中的静态 key 不会与 Session 混用；CLI/Docker 仍走 Start。
func StartSession(o cfg.Options, session *SessionConfig) error {
	opts, err := optionsOf(o, cfg.DeviceType, cfg.Version, func(format string, args ...any) {
		logv.WithNoCaller.Info().Msgf(format, args...)
	})
	if err != nil {
		return err
	}
	opts.Key = ""
	opts.Session = session
	return StartOptions(opts)
}

// StartOptions 以明确的静态或 Session 配置启动进程内唯一 Client。
func StartOptions(opts Options) error {
	return startOptions(opts, Connect)
}

func startOptions(opts Options, connect connectClientFunc) error {
	rtMu.Lock()
	defer rtMu.Unlock()
	if rtClient != nil {
		return fmt.Errorf("host already running")
	}
	c, err := connect(opts)
	if err != nil {
		noteOptionsFailure(opts, err)
		return err
	}
	rtClient = c
	return nil
}

// SwitchAccount 完整销毁旧账户 Client 后再创建新 Client。
// 旧 NATS inbox、RTC、会话 grant、后台进程、命令服务实例和 token provider 都不会跨账户复用。
// 新连接失败时旧连接也不会复活；调用方应展示错误并允许重试或重新登录。
func SwitchAccount(opts Options) error {
	return switchAccount(opts, Connect)
}

func switchAccount(opts Options, connect connectClientFunc) error {
	rtMu.Lock()
	defer rtMu.Unlock()
	stopRuntimeClientLocked()
	c, err := connect(opts)
	if err != nil {
		noteOptionsFailure(opts, err)
		return err
	}
	rtClient = c
	return nil
}

// Stop 停止 host 会话（状态落盘为未连接：桌面据此看到「未连接」而不是残留的已连接）。
func Stop() {
	rtMu.Lock()
	defer rtMu.Unlock()
	stopRuntimeClientLocked()
}

func stopRuntimeClientLocked() {
	if rtClient == nil {
		return
	}
	id, version := rtClient.hostID, rtClient.options().Version
	_ = rtClient.Close()
	rtClient = nil
	writeState(State{Connected: false, HostID: id, Version: version, LastError: "host session stopped"})
}

func noteOptionsFailure(opts Options, err error) {
	if err == nil {
		return
	}
	hostID := ""
	if identity, identityErr := identityFromOptions(opts); identityErr == nil {
		hostID = identity.hostID
	}
	writeState(State{HostID: hostID, Version: opts.Version, LastError: err.Error()})
}
