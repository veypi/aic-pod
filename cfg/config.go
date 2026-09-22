// Copyright (C) 2025 veypi <i@veypi.com>
// Distributed under terms of the MIT license.

// Package cfg 是 aic-pod 的配置中心（cli/desktop 共用同一份配置）：
// Options 结构体 + Global 全局有效配置，落盘 UserConfigDir/aic/config.yaml。
package cfg

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"github.com/veypi/aic-pod/libs/policy"
	"github.com/veypi/vigo/flags"
	"gopkg.in/natefinch/lumberjack.v2"
)

// DefaultHost 是默认平台地址。
const DefaultHost = "https://ivec-ai.com"

// Version 客户端版本：Makefile -X github.com/veypi/aic-pod/cfg.Version 注入 git
// 版本，未注入时以此兑底。发版只改本变量；desktop 版本由构建同步。
var Version = "v0.8.2"

// DeviceType 客户端类型（cli/desktop），启动时固定（desktop main 覆盖为 "desktop"）。
var DeviceType = "cli"

// Options 是 cli 与 desktop 共享的唯一配置模型（配置参数就是一个结构体，
// vigo/flags AutoRegister/LoadCfg/DumpCfg 直接使用）：
//
//   - json tag：flag 名（-host/-key/-work_dir/-exec_timeout/-home_path）与 env 键
//     （HOST/KEY/WORK_DIR/EXEC_TIMEOUT/HOME_PATH）的来源，也是设置面（settings 包，
//     `aic config get|set`）的键
//   - yaml tag：配置文件的键（与 json tag 同名 snake_case）；未知字段忽略，错误授权字段阻止工具调用
//   - default tag：结构体默认值（无文件无 env 无 flag 时生效）
//   - desc tag：-h 帮助文案
//
// 落盘位置：os.UserConfigDir()/aic/config.yaml（flags.DumpCfg，原子写），
// cli 与 desktop 读写同一份——任一端的修改（编辑文件 / 页面绑定）另一端启动即生效。
//
// 解析优先级：显式 flag > 环境变量 > 配置文件（flags.LoadCfg）> default tag
type Options struct {
	Host        string `json:"host" yaml:"host" default:"https://ivec-ai.com" desc:"platform address (NATS endpoint inferred)"`
	Key         string `json:"key" yaml:"key" desc:"binding credential key (from platform device page)"`
	WorkDir     string `json:"work_dir" yaml:"work_dir" desc:"working directory for exec (default: system temp dir)"`
	ExecTimeout string `json:"exec_timeout" yaml:"exec_timeout" default:"30m" desc:"exec background timeout"`
	// HomePath 默认打开地址（desktop 启动/托盘打开时加载 host+HomePath）：
	// 必须为 / 开头的路径（如 /、/a、/agents），默认 /。
	HomePath string `json:"home_path" yaml:"home_path" default:"/" desc:"default page path to open on platform (must start with /)"`
	// Browser viewport applies to newly created Chrome pages, independently of display layout.
	BrowserPath   string `json:"browser_path" yaml:"browser_path" desc:"Chrome executable (independent of Electron)"`
	BrowserWidth  int    `json:"browser_width" yaml:"browser_width" default:"1280" desc:"browser viewport width in pixels (320-4096)"`
	BrowserHeight int    `json:"browser_height" yaml:"browser_height" default:"720" desc:"browser viewport height in pixels (320-4096)"`
	// NoSandbox 全局禁用 exec 进程沙箱（§5.10）：缺省 false = 沙箱开启；
	// 置 true 后所有 exec 调用跳过沙箱包装（与请求级 nosandbox 同效，无需审批）。
	// 慎用：等同放弃进程级隔离（仅建议本机可信环境）。
	NoSandbox bool `json:"no_sandbox" yaml:"no_sandbox" desc:"disable process sandbox for exec calls (default: sandbox enabled)"`
	// RTC 直连应答开关（WebRTC DataChannel，2026-09-10）：开启后 host 作为
	// 应答方接受 owner 页面发起的 RTC 直连（信令经 NATS，数据面 UDP/DTLS），
	// 能力摘要按 rtc/proxy 分别上报，凭据不进入摘要。
	// 关闭 RTC 仍保留服务器文件 proxy。
	RTC bool `json:"rtc" yaml:"rtc" default:"true" desc:"answer WebRTC direct links from owner pages (default true)"`

	// Zero values use the advertised device defaults (512 MiB / 64 MiB / 128).
	HostsUploadBytes      int64 `json:"hosts_upload_bytes,omitempty" yaml:"hosts_upload_bytes" desc:"maximum hosts upload size in bytes"`
	HostsProxyUploadBytes int64 `json:"hosts_proxy_upload_bytes,omitempty" yaml:"hosts_proxy_upload_bytes" desc:"maximum proxied hosts upload size in bytes"`
	HostsSources          int   `json:"hosts_sources,omitempty" yaml:"hosts_sources" desc:"maximum retained filesystem byte sources"`

	// Execution policies: deny first, then operation-covering allow, then default.
	ExecPolicy string   `json:"exec_policy" yaml:"exec_policy" default:"open" desc:"registered command stance: deny | open"`
	ExecDeny   []string `json:"exec_deny" yaml:"exec_deny" desc:"denied registered command names or *"`
	ExecAllow  []string `json:"exec_allow" yaml:"exec_allow" desc:"allowed registered command names or *"`
	FsPolicy   string   `json:"fs_policy" yaml:"fs_policy" default:"deny" desc:"fs write stance: deny (writable via rules/roots only) | open (all writes except deny rules); reads are open except deny rules"`
	FsRules    []string `json:"fs_rules" yaml:"fs_rules" desc:"ordered fs rules: 'deny:|ro:|rw:' + path glob, last match wins; bare path covers its subtree; global patterns are rejected; permanent grants append here"`
	NetPolicy  string   `json:"net_policy" yaml:"net_policy" default:"open" desc:"sandboxed process outbound stance: open (default) | deny (localhost-only lockdown)"`
	NetRules   []string `json:"net_rules" yaml:"net_rules" desc:"ordered net rules: 'allow:|deny:' + host[:port], last match wins (builtin allow localhost:* stays first); permanent grants append here"`
	SshPolicy  string   `json:"ssh_policy" yaml:"ssh_policy" default:"deny" desc:"ssh tool target stance: deny | open"`
	SshRules   []string `json:"ssh_rules" yaml:"ssh_rules" desc:"ordered ssh rules: 'allow:|deny:' + host[:port], last match wins; permanent grants append here"`
}

// Global 全局有效配置：NewOptions 初始化 → Load 填充文件值 →
// flags.AutoRegister(Global) 叠加 flag/env；设置面的写操作（settings.Update.Apply）
// 落盘后由调用方重启进程生效。
var Global = NewOptions()

// NewOptions 返回带默认值的配置实例。
func NewOptions() *Options {
	o := &Options{}
	flags.SetDefaults(o)
	return o
}

// 授权策略取值（fs_policy/exec_policy/net_policy/ssh_policy 的合法值）。
const (
	PolicyDeny = "deny" // 仅 allow 放行
	PolicyOpen = "open" // 除 deny 全放
)

// NormalizePolicy 归一授权策略取值：空 → def（域默认）；非法值一律 deny（安全侧失败）。
func NormalizePolicy(s, def string) string {
	if s == PolicyOpen || s == PolicyDeny {
		return s
	}
	if s == "" {
		return def
	}
	return PolicyDeny
}

// Normalize repairs ordinary settings, but preserves malformed authorization
// values so they cannot silently become permissions or be erased by Save.
func (o *Options) Normalize() {
	h := strings.TrimSpace(o.Host)
	if h != "" && !strings.Contains(h, "://") {
		h = "https://" + h
	}
	u, err := url.Parse(h)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
		o.Host = DefaultHost
	} else {
		o.Host = h
	}
	if d, err := time.ParseDuration(o.ExecTimeout); err != nil || d <= 0 {
		o.ExecTimeout = "30m"
	}
	o.HomePath = o.NormalizedHomePath()
	if o.BrowserWidth < 320 || o.BrowserWidth > 4096 {
		o.BrowserWidth = 1280
	}
	if o.BrowserHeight < 320 || o.BrowserHeight > 4096 {
		o.BrowserHeight = 720
	}
	// 类型解析由 flags 完成；这里只处理设备权限规则的业务语义。
	for _, mode := range []struct {
		value    *string
		fallback string
	}{
		{&o.ExecPolicy, PolicyOpen}, {&o.FsPolicy, PolicyDeny},
		{&o.NetPolicy, PolicyOpen}, {&o.SshPolicy, PolicyDeny},
	} {
		if *mode.value == "" {
			*mode.value = mode.fallback
		}
	}
}

// ApplyConfigIssues prevents the generic parser's permissive fallback from
// enabling tools after an authorization field (or the entire file) failed to
// decode. Invalid markers must be explicitly replaced before saving; the file
// itself remains untouched and the local management API can still start.
func (o *Options) ApplyConfigIssues(issues []flags.ConfigIssue) {
	fields := map[string]any{
		"exec_policy": &o.ExecPolicy, "exec_allow": &o.ExecAllow, "exec_deny": &o.ExecDeny,
		"fs_policy": &o.FsPolicy, "fs_rules": &o.FsRules,
		"net_policy": &o.NetPolicy, "net_rules": &o.NetRules,
		"ssh_policy": &o.SshPolicy, "ssh_rules": &o.SshRules,
	}
	for _, issue := range issues {
		for name, field := range fields {
			if issue.Field != "" && issue.Field != name {
				continue
			}
			switch value := field.(type) {
			case *string:
				*value = "invalid"
			case *[]string:
				// A visible invalid entry survives the settings editor's trimming
				// of blank lines; unrelated form saves must not clear this error.
				*value = []string{"INVALID " + name + ": repair malformed configuration"}
			}
		}
	}
}

// NormalizedHomePath 返回规范化默认首页路径：空 → "/"；非 / 开头补 "/"；
// "//" 开头（协议相对 URL 形态，拼接后会被浏览器解析到别的站点）→ "/"。
func (o *Options) NormalizedHomePath() string {
	p := strings.TrimSpace(o.HomePath)
	if p == "" || strings.HasPrefix(p, "//") {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		return "/" + p
	}
	return p
}

// HomeURL 返回默认打开地址 {host}{home_path}（host 可带产品壳路径前缀，
// 如 http://127.0.0.1:4000/rses/aiv + /a → http://127.0.0.1:4000/rses/aiv/a）。
func (o *Options) HomeURL() string {
	h := strings.TrimSpace(o.Host)
	if h == "" {
		h = DefaultHost
	}
	if !strings.Contains(h, "://") {
		h = "https://" + h
	}
	u, err := url.Parse(h)
	if err != nil || u.Host == "" {
		return h + o.NormalizedHomePath()
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + o.NormalizedHomePath()
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

// Path 返回配置文件路径：UserConfigDir/aic/config.yaml。
func Path() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "aic", "config.yaml"), nil
}

// StateDir 返回工具状态根目录：UserConfigDir/aic（与 config.yaml/log 同根）。
// 工具自身的持久状态（browser 数据目录等，不暴露给 AI）一律放这里，
// 禁止落用户工作区（workdir 可能是 git 仓库）。
func StateDir() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "aic"), nil
}

// PublicDir 返回公共可写区根目录：$HOME/.aic（用户家目录下，首次调用时创建，
// 0700）。两类使用方共享同一事实源：
//   - exec 进程沙箱 workspace-write 白名单（§5.10）：沙箱内命令可写公共区；
//   - 工具状态保存（browser state save 等）：AI 与工具共享的跨会话保存区。
func PublicDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	p := filepath.Join(home, ".aic")
	if err := os.MkdirAll(p, 0o700); err != nil {
		return "", err
	}
	return p, nil
}

// LogPath 返回日志文件路径：UserConfigDir/aic/aic.log（get_log 的数据源）。
func LogPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "aic", "aic.log"), nil
}

// LogWriter 返回日志文件 writer（console 格式无色，lumberjack 滚动 16MB×3）。
// cli 与 ConsoleWriter 双写；desktop 仅文件。
func LogWriter() (io.Writer, error) {
	p, err := LogPath()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, err
	}
	w := zerolog.NewConsoleWriter()
	w.Out = &lumberjack.Logger{Filename: p, MaxSize: 16, MaxBackups: 3, LocalTime: true}
	w.NoColor = true
	return w, nil
}

// LoadFile 仅读取配置文件返回独立副本，不触碰 Global——
// 设置面与 bind/unbind 子命令落盘用：基于文件配置修改，flag/env 启动覆盖不落盘。
// 配置文件不阻断启动和设置：普通字段错误回退默认值；授权字段错误或整体
// 损坏保留无效标记，阻止设备工具调用（fail-closed）。保存路径不以文件内容为门：
// 任何保存都落盘，坏内容的可见性由 INVALID 标记承担。废弃授权键（fs_deny 等）
// 直接失效（不做迁移），随下次重写自然清除。
func LoadFile() (*Options, error) {
	o := NewOptions()
	if p, err := Path(); err == nil {
		o.ApplyConfigIssues(flags.LoadCfg(p, o))
	}
	o.Normalize()
	return o, nil
}

// Load 安装配置；无效授权仍可通过设置面（aic config set）修复。
func Load() (*Options, error) {
	o, err := LoadFile()
	if err == nil {
		Global = o
	}
	return o, err
}

// authMu 守护执行策略配置的并发读写：api.SetConfig（用户操作）与
// host grant --permanent（AI 经审批）两条写入路径共用。
var authMu sync.RWMutex

// AuthCfg 是四域执行策略快照（exec 保持 policy/deny/allow 三键；
// fs/net/ssh 为 policy + 有序规则表 rules——permanent grant 直接追加在 rules 表尾）。
type AuthCfg struct {
	ExecPolicy string
	ExecDeny   []string
	ExecAllow  []string
	FsPolicy   string
	FsRules    []string
	NetPolicy  string
	NetRules   []string
	SshPolicy  string
	SshRules   []string
}

// AuthSnapshot 返回当前授权配置快照（fsauth/netauth Reconcile 的数据源）。
// policy 在读点归一化（防空值/非法值漂移到安全语义外：fs/ssh 空=deny，net 空=open）。
func AuthSnapshot() AuthCfg {
	c := RawAuthSnapshot()
	c.ExecPolicy = NormalizePolicy(c.ExecPolicy, PolicyOpen)
	c.FsPolicy = NormalizePolicy(c.FsPolicy, PolicyDeny)
	c.NetPolicy = NormalizePolicy(c.NetPolicy, PolicyOpen)
	c.SshPolicy = NormalizePolicy(c.SshPolicy, PolicyDeny)
	return c
}

// RawAuthSnapshot exposes malformed fields to the local settings editor so
// saving unrelated settings cannot mistake a fallback for an explicit repair.
func RawAuthSnapshot() AuthCfg {
	authMu.RLock()
	defer authMu.RUnlock()
	return AuthFrom(Global)
}

// CheckAuth gates device tools independently of grants and permission levels.
// A broken local policy must be repaired through the settings surface (aic config set).
func CheckAuth() error {
	authMu.RLock()
	defer authMu.RUnlock()
	return Global.ValidateAuth()
}

// SetAuth 更新授权配置（内存即时生效；落盘由调用方负责——
// settings.Update.Apply 走 Save，grant --permanent 亦同）。
func SetAuth(c AuthCfg) {
	authMu.Lock()
	defer authMu.Unlock()
	Global.ExecPolicy, Global.ExecDeny, Global.ExecAllow = NormalizePolicy(c.ExecPolicy, PolicyOpen), c.ExecDeny, c.ExecAllow
	Global.FsPolicy, Global.FsRules = NormalizePolicy(c.FsPolicy, PolicyDeny), c.FsRules
	Global.NetPolicy, Global.NetRules = NormalizePolicy(c.NetPolicy, PolicyOpen), c.NetRules
	Global.SshPolicy, Global.SshRules = NormalizePolicy(c.SshPolicy, PolicyDeny), c.SshRules
}

// AuthFrom 从 Options 取授权快照（SetAuth 的入参装配）。
func AuthFrom(o *Options) AuthCfg {
	return AuthCfg{
		ExecPolicy: o.ExecPolicy, ExecDeny: o.ExecDeny, ExecAllow: o.ExecAllow,
		FsPolicy: o.FsPolicy, FsRules: o.FsRules,
		NetPolicy: o.NetPolicy, NetRules: o.NetRules,
		SshPolicy: o.SshPolicy, SshRules: o.SshRules,
	}
}

// Save 持久化配置（yaml，flags.DumpCfg 原子写；含凭证，文件权限 0600）。
// 不以文件内容为门：任何保存都落盘——文件里已有什么只影响运行时工具门控，
// 不构成拒绝写入的理由；废弃键与未知键随重写自然消失。
func Save(o *Options) error {
	p, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	o.Normalize()
	if err := flags.DumpCfg(p, o); err != nil {
		return err
	}
	// DumpCfg 以 0644 创建，凭证敏感改 0600
	return os.Chmod(p, 0o600)
}

// ValidateAuth rejects malformed execution rules before they become active.
func (o *Options) ValidateAuth() error {
	for _, mode := range []string{o.FsPolicy, o.ExecPolicy, o.NetPolicy, o.SshPolicy} {
		if mode != "" && mode != PolicyOpen && mode != PolicyDeny {
			return fmt.Errorf("invalid policy %q", mode)
		}
	}
	if err := policy.ValidateFSRules(o.FsRules); err != nil {
		return err
	}
	if err := policy.ValidateExec(o.ExecAllow); err != nil {
		return err
	}
	if err := policy.ValidateExec(o.ExecDeny); err != nil {
		return err
	}
	for _, list := range [][]string{o.NetRules, o.SshRules} {
		if err := policy.ValidateTargetRules(list); err != nil {
			return err
		}
	}
	return nil
}

// LockUpdate serializes local config read-modify-write transactions.
func LockUpdate() func() { configUpdateMu.Lock(); return configUpdateMu.Unlock }

var configUpdateMu sync.Mutex
