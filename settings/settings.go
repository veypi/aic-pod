// Copyright (C) 2025 veypi <i@veypi.com>
// Distributed under terms of the MIT license.

// Package settings 是本机设置面（原 api 包 get_config/set_config 的逻辑，2026-09-22
// 随本地管理 API 一并从 HTTP 端点改为进程内调用）：
//   - cli 的 `aic config get|set` 子命令（Electron 设置窗口经主进程 spawn 调用）
//   - 读写同一份 config.yaml；Apply 只落盘，生效由调用方重启后端进程完成
package settings

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/netauth"
)

// View 是设置面读视图（含 key——设置窗口需显示当前凭证；仅本机同用户进程可见）。
type View struct {
	Version       string   `json:"version"`
	BrowserPath   string   `json:"browser_path"`
	BrowserWidth  int      `json:"browser_width"`
	BrowserHeight int      `json:"browser_height"`
	Host          string   `json:"host"`
	Key           string   `json:"key"`
	WorkDir       string   `json:"work_dir"`
	ExecTimeout   string   `json:"exec_timeout"`
	HomePath      string   `json:"home_path"`
	ExecPolicy    string   `json:"exec_policy"`
	ExecDeny      []string `json:"exec_deny"`
	ExecAllow     []string `json:"exec_allow"`
	FsPolicy      string   `json:"fs_policy"`
	FsDeny        []string `json:"fs_deny"`
	FsAllow       []string `json:"fs_allow"`
	NetPolicy     string   `json:"net_policy"`
	NetDeny       []string `json:"net_deny"`
	NetAllow      []string `json:"net_allow"`
	SshPolicy     string   `json:"ssh_policy"`
	SshDeny       []string `json:"ssh_deny"`
	SshAllow      []string `json:"ssh_allow"`
}

// Snapshot 返回当前有效配置（cfg.Global：启动解析值；caller 需已 cfg.Load）。
// 坏授权字段保持原样可见（供显式修复），不显示成已归一化的策略。
// 隐藏配置（no_sandbox 等）不在此视图——仅配置文件/flag/env 可配。
func Snapshot() *View {
	o := cfg.Global
	a := cfg.RawAuthSnapshot()
	return &View{Version: cfg.Version,
		Host: o.Host, Key: o.Key, WorkDir: o.WorkDir, ExecTimeout: o.ExecTimeout,
		HomePath:    o.NormalizedHomePath(),
		BrowserPath: o.BrowserPath, BrowserWidth: o.BrowserWidth, BrowserHeight: o.BrowserHeight,
		ExecPolicy: a.ExecPolicy, ExecDeny: a.ExecDeny, ExecAllow: a.ExecAllow,
		FsPolicy: a.FsPolicy, FsDeny: a.FsDeny, FsAllow: a.FsAllow,
		NetPolicy: a.NetPolicy, NetDeny: a.NetDeny, NetAllow: a.NetAllow,
		SshPolicy: a.SshPolicy, SshDeny: a.SshDeny, SshAllow: a.SshAllow}
}

// Update 是设置面写请求白名单（host/work_dir/exec_timeout/home_path/browser_*
// 与授权十二键可写；key 不走这里——只走 bind/unbind 子命令）。
// 隐藏配置（no_sandbox 等）不可经此修改，只能改配置文件。
// 授权十二键（policy/deny/allow × exec/fs/net/ssh）：policy 空串 = 不改；列表 nil = 不改
// （保持现状），非 nil（含空数组）= 整体替换——空数组即清空，是 grant --permanent
// 的唯一回撤出口。
type Update struct {
	BrowserPath   *string   `json:"browser_path"`
	BrowserWidth  *int      `json:"browser_width"`
	BrowserHeight *int      `json:"browser_height"`
	Host          string    `json:"host"`
	WorkDir       string    `json:"work_dir"`
	ExecTimeout   string    `json:"exec_timeout"`
	HomePath      string    `json:"home_path"`
	ExecPolicy    string    `json:"exec_policy"`
	ExecDeny      *[]string `json:"exec_deny"`
	ExecAllow     *[]string `json:"exec_allow"`
	FsPolicy      string    `json:"fs_policy"`
	FsDeny        *[]string `json:"fs_deny"`
	FsAllow       *[]string `json:"fs_allow"`
	NetPolicy     string    `json:"net_policy"`
	NetDeny       *[]string `json:"net_deny"`
	NetAllow      *[]string `json:"net_allow"`
	SshPolicy     string    `json:"ssh_policy"`
	SshDeny       *[]string `json:"ssh_deny"`
	SshAllow      *[]string `json:"ssh_allow"`
}

// validPolicy 校验 policy 取值（空串 = 不改，合法）。
func validPolicy(s string) bool {
	return s == "" || s == cfg.PolicyDeny || s == cfg.PolicyOpen
}

// Apply 校验并持久化设置：基于文件配置落盘（flag/env 启动覆盖不落盘）。
// 只写 config.yaml，不碰运行中进程的内存态——生效由调用方重启后端完成。
func (u *Update) Apply() error {
	unlock := cfg.LockUpdate()
	defer unlock()
	for name, size := range map[string]*int{"browser_width": u.BrowserWidth, "browser_height": u.BrowserHeight} {
		if size != nil && (*size < 320 || *size > 4096) {
			return &InvalidArg{Field: name, Reason: "must be between 320 and 4096"}
		}
	}
	if s := strings.TrimSpace(u.ExecTimeout); s != "" {
		if _, err := time.ParseDuration(s); err != nil {
			return &InvalidArg{Field: "exec_timeout", Reason: err.Error()}
		}
	}
	// 授权配置显式校验；坏配置阻止设备工具调用，必须修正后才能保存。
	if !validPolicy(u.ExecPolicy) || !validPolicy(u.FsPolicy) || !validPolicy(u.NetPolicy) || !validPolicy(u.SshPolicy) {
		return &InvalidArg{Field: "policy", Reason: "want deny | open"}
	}
	for name, list := range map[string]*[]string{"net_deny": u.NetDeny, "net_allow": u.NetAllow, "ssh_deny": u.SshDeny, "ssh_allow": u.SshAllow} {
		if list != nil {
			if err := netauth.ValidateEntries(*list); err != nil {
				return &InvalidArg{Field: name, Reason: err.Error()}
			}
		}
	}
	fileCfg, err := cfg.LoadFile()
	if err != nil {
		return err
	}
	if h := strings.TrimSpace(u.Host); h != "" {
		fileCfg.Host = h
	}
	// work_dir：~ 展开 + 归一绝对路径 + 有效性校验，落盘即真实路径
	//（Go exec 不做 shell 展开，设置面填 ~/test 会因路径不存在导致所有 exec 失败）
	wd := strings.TrimSpace(u.WorkDir)
	if wd != "" {
		wd = expandHome(wd)
		abs, err := filepath.Abs(wd)
		if err != nil {
			return &InvalidArg{Field: "work_dir", Reason: err.Error()}
		}
		st, err := os.Stat(abs)
		if err != nil || !st.IsDir() {
			return &InvalidArg{Field: "work_dir", Reason: "not a directory: " + abs}
		}
		wd = abs
	}
	if u.BrowserPath != nil {
		fileCfg.BrowserPath = strings.TrimSpace(*u.BrowserPath)
	}
	if u.BrowserWidth != nil {
		fileCfg.BrowserWidth = *u.BrowserWidth
	}
	if u.BrowserHeight != nil {
		fileCfg.BrowserHeight = *u.BrowserHeight
	}
	fileCfg.WorkDir = wd
	fileCfg.ExecTimeout = strings.TrimSpace(u.ExecTimeout)
	applyAuth(u, fileCfg)
	// home_path：必须以单个 / 开头（// 开头是协议相对 URL，拼接后会跳转到别的站点，拒绝）
	if hp := strings.TrimSpace(u.HomePath); hp != "" {
		if !strings.HasPrefix(hp, "/") || strings.HasPrefix(hp, "//") {
			return &InvalidArg{Field: "home_path", Reason: "must be a path starting with / (e.g. / or /a)"}
		}
		fileCfg.HomePath = hp
	} else {
		fileCfg.HomePath = "/" // 清空 = 恢复默认首页
	}
	if err := fileCfg.ValidateAuth(); err != nil {
		return err
	}
	return cfg.Save(fileCfg)
}

// InvalidArg 是设置面参数错误（前端按 field/reason 展示）。
type InvalidArg struct {
	Field  string
	Reason string
}

func (e *InvalidArg) Error() string { return "invalid " + e.Field + ": " + e.Reason }

func applyAuth(u *Update, o *cfg.Options) {
	for _, field := range []struct {
		value  string
		target *string
	}{
		{u.ExecPolicy, &o.ExecPolicy}, {u.FsPolicy, &o.FsPolicy},
		{u.NetPolicy, &o.NetPolicy}, {u.SshPolicy, &o.SshPolicy},
	} {
		if field.value != "" {
			*field.target = field.value
		}
	}
	for _, field := range []struct{ value, target *[]string }{
		{u.ExecAllow, &o.ExecAllow}, {u.ExecDeny, &o.ExecDeny},
		{u.FsAllow, &o.FsAllow}, {u.FsDeny, &o.FsDeny},
		{u.NetAllow, &o.NetAllow}, {u.NetDeny, &o.NetDeny},
		{u.SshAllow, &o.SshAllow}, {u.SshDeny, &o.SshDeny},
	} {
		if field.value != nil {
			*field.target = *field.value
		}
	}
}

// expandHome 展开 work_dir 的 ~ 前缀（~ 或 ~/xxx → 用户主目录）。
// 配置保存时调用：落盘即为真实绝对路径，运行时不再需要 shell 展开语义。
func expandHome(p string) string {
	if p == "~" {
		if h, err := os.UserHomeDir(); err == nil {
			return h
		}
		return p
	}
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, strings.TrimPrefix(p, "~/"))
		}
	}
	return p
}

// BoundHostID 从凭证首段解析 host_id（与连接状态无关）。
func BoundHostID(credential string) string {
	parts := strings.Split(strings.TrimSpace(credential), ".")
	if len(parts) == 4 {
		return parts[0]
	}
	return ""
}
