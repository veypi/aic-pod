package host

import (
	"fmt"
	"strings"
	"sync"

	"github.com/veypi/vigo/logv"
)

// Runner 是 host 会话生命周期管理（cli/desktop 共用）。
// 日志统一走 vigo/logv（不建自有日志系统），get_log 由挂入 logv 的
// RingBuffer 提供（见 NewRingBuffer）。
type Runner struct {
	mu         sync.Mutex
	client     *Client
	deviceType string
	version    string
}

// NewRunner 创建 Runner。deviceType 为客户端类型（cli/desktop），
// version 为客户端版本（va.b.c，服务端主版本门禁）。
func NewRunner(deviceType, version string) *Runner {
	return &Runner{deviceType: deviceType, version: version}
}

// StartHost 启动 host 会话（凭证为空返回错误；已运行返回 host already running）。
func (r *Runner) StartHost(cfg Config) error {
	r.mu.Lock()
	if r.client != nil {
		r.mu.Unlock()
		return fmt.Errorf("host already running")
	}
	r.mu.Unlock()
	if strings.TrimSpace(cfg.Key) == "" {
		return fmt.Errorf("key is empty — bind a device first")
	}
	opts, err := cfg.Options(r.deviceType, r.version, func(format string, args ...any) {
		logv.WithNoCaller.Info().Msgf(format, args...)
	})
	if err != nil {
		return err
	}
	c, err := Connect(opts)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.client = c
	r.mu.Unlock()
	return nil
}

// StopHost 停止 host 会话。
func (r *Runner) StopHost() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.client != nil {
		r.client.Close()
		r.client = nil
	}
}

// Running 报告 host 会话是否在运行。
func (r *Runner) Running() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.client != nil
}

// OpenPlatformURL 打开平台 URL：cli 无窗口，用系统默认浏览器打开（desktop 应用内跳转）。
func (r *Runner) OpenPlatformURL(url string) error {
	return OpenExternal(url)
}

// ApplyConfig 应用新运行配置（保存设置后调用）：保留会话与 bg 任务，
// 仅更新参数；NATS 地址变化时重连（Client.Reconfigure）。未运行则直接启动。
func (r *Runner) ApplyConfig(cfg Config) error {
	r.mu.Lock()
	c := r.client
	r.mu.Unlock()
	if c == nil {
		return r.StartHost(cfg)
	}
	return c.Reconfigure(cfg)
}
