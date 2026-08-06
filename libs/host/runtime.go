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
	rtMu   sync.Mutex
	rtClient *Client
)

// Start 启动 host 会话（凭证为空返回错误；已运行返回 host already running）。
// 客户端身份取 cfg.DeviceType/cfg.Version（二进制启动时固定）。
func Start(o cfg.Options) error {
	rtMu.Lock()
	if rtClient != nil {
		rtMu.Unlock()
		return fmt.Errorf("host already running")
	}
	rtMu.Unlock()
	if strings.TrimSpace(o.Key) == "" {
		return fmt.Errorf("key is empty — bind a device first")
	}
	opts, err := optionsOf(o, cfg.DeviceType, cfg.Version, func(format string, args ...any) {
		logv.WithNoCaller.Info().Msgf(format, args...)
	})
	if err != nil {
		return err
	}
	c, err := Connect(opts)
	if err != nil {
		return err
	}
	rtMu.Lock()
	rtClient = c
	rtMu.Unlock()
	return nil
}

// Stop 停止 host 会话。
func Stop() {
	rtMu.Lock()
	defer rtMu.Unlock()
	if rtClient != nil {
		rtClient.Close()
		rtClient = nil
	}
}

// Running 报告 host 会话是否在运行。
func Running() bool {
	rtMu.Lock()
	defer rtMu.Unlock()
	return rtClient != nil
}

// ApplyConfig 应用新运行配置（保存设置后调用）：保留会话与 bg 任务，
// 仅更新参数；NATS 地址变化时重连（Client.Reconfigure）。未运行则直接启动。
func ApplyConfig(o cfg.Options) error {
	rtMu.Lock()
	c := rtClient
	rtMu.Unlock()
	if c == nil {
		return Start(o)
	}
	return c.Reconfigure(o)
}
