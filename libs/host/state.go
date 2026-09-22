package host

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/veypi/aic-pod/cfg"
)

// State 是 host 会话的对外连接状态快照（2026-09-23「重连假成功」修复）：
// 桌面设置窗读 {UserConfigDir}/aic/state.json 判真实连接，而不是「子进程
// 存活 + key 非空」。后端在连接成功 / 断开 / 认证失败 / 退避重试失败时原子
// 更新；PID 供桌面核对是否为当前子进程所写（防陈旧文件误读）。
type State struct {
	PID       int    `json:"pid"`
	Connected bool   `json:"connected"`
	HostID    string `json:"host_id,omitempty"`
	NATSURL   string `json:"nats_url,omitempty"`
	Version   string `json:"version,omitempty"`
	LastError string `json:"last_error,omitempty"`
	Retrying  bool   `json:"retrying,omitempty"`
	UpdatedAt string `json:"updated_at"`
}

// writeState 原子写状态快照（tmp + rename；失败静默——状态上报是尽力而为，
// 不阻塞/不打断连接主流程）。PID/UpdatedAt 由本函数统一填充。
func writeState(s State) {
	dir, err := cfg.StateDir()
	if err != nil {
		return
	}
	s.PID = os.Getpid()
	s.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	b, err := json.Marshal(s)
	if err != nil {
		return
	}
	p := filepath.Join(dir, "state.json")
	tmp := p + ".tmp"
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, p)
}

// NoteStartFailure 记录初次连接失败（退避重试期间桌面据此显示「未连接 +
// 原因（重试中）」而不是假「已连接」）。retrying=false = 永久错误中止重试。
func NoteStartFailure(o cfg.Options, err error, retrying bool) {
	if err == nil {
		return
	}
	writeState(State{HostID: hostIDOf(o.Key), Version: cfg.Version, LastError: err.Error(), Retrying: retrying})
}

// hostIDOf 从凭证首段解析 host_id（与 settings.BoundHostID 同口径；无凭证为空）。
func hostIDOf(key string) string {
	parts := strings.SplitN(strings.TrimSpace(key), ".", 4)
	if len(parts) == 4 {
		return parts[0]
	}
	return ""
}
