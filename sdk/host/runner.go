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
