// Copyright (C) 2025 veypi <i@veypi.com>
// Distributed under terms of the MIT license.

// Package cfg 是 aic-pod 的配置中心（cli/desktop 共用同一份配置）：
// Options 结构体 + Global 全局有效配置，落盘 UserConfigDir/aic/config.yaml。
package cfg

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/veypi/aic-pod/libs/policy"
	"gopkg.in/yaml.v3"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/rs/zerolog"
	"github.com/veypi/vigo/flags"
	"gopkg.in/natefinch/lumberjack.v2"
)

// DefaultHost 是默认平台地址。
const DefaultHost = "https://ivec-ai.com"

// Version 客户端版本：Makefile -X github.com/veypi/aic-pod/cfg.Version 注入 git
// 版本，未注入时以此兑底。发版只改本变量；desktop 版本由构建同步。
var Version = "v0.6.7"

// DeviceType 客户端类型（cli/desktop），启动时固定（desktop main 覆盖为 "desktop"）。
var DeviceType = "cli"

// Options 是 cli 与 desktop 共享的唯一配置模型（配置参数就是一个结构体，
// vigo/flags AutoRegister/LoadCfg/DumpCfg 直接使用）：
//
//   - json tag：flag 名（-host/-key/-work_dir/-exec_timeout/-home_path/-code）与 env 键
//     （HOST/KEY/WORK_DIR/EXEC_TIMEOUT/HOME_PATH/CODE）的来源，也是本地 API（get_config/
//     set_config）的键
//   - yaml tag：配置文件的键（与 json tag 同名 snake_case）；历史落盘形态（结构体默认
//     小写字段名，如 fsdeny/workdir）在 UnmarshalYAML 里兼容读入，Save 后自愈为新形态
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
	// Browser viewport applies to newly created desktop tabs, independently of display layout.
	BrowserWidth  int `json:"browser_width" yaml:"browser_width" default:"1280" desc:"browser viewport width in pixels (320-4096)"`
	BrowserHeight int `json:"browser_height" yaml:"browser_height" default:"720" desc:"browser viewport height in pixels (320-4096)"`
	// NoSandbox 全局禁用 exec 进程沙箱（§5.10）：缺省 false = 沙箱开启；
	// 置 true 后所有 exec 调用跳过沙箱包装（与请求级 nosandbox 同效，无需审批）。
	// 慎用：等同放弃进程级隔离（仅建议本机可信环境）。
	NoSandbox bool `json:"no_sandbox" yaml:"no_sandbox" desc:"disable process sandbox for exec calls (default: sandbox enabled)"`
	// Code 本地 API 校验码（x-aic-code 头，纯随机秘钥，与端口无关）：
	// 可配置（config.yaml 写死则固定，重启不失效）；为空时启动随机生成，
	// 自动生成的值不写回配置文件（生命周期 = 进程，重启换新）。
	Code string `json:"code" yaml:"code" desc:"local api secret code (empty = random per process)"`
	// RTC 直连应答开关（WebRTC DataChannel，2026-09-10）：开启后 host 作为
	// 应答方接受 owner 页面发起的 RTC 直连（信令经 NATS，数据面 UDP/DTLS），
	// 能力摘要按 rtc/proxy 分别上报，凭据不进入摘要。
	// 关闭 RTC 仍保留服务器文件 proxy。
	RTC bool `json:"rtc" yaml:"rtc" default:"true" desc:"answer WebRTC direct links from owner pages (default true)"`

	// Zero values use the advertised device defaults (512 MiB / 64 MiB / 4).
	HostsUploadBytes      int64 `json:"hosts_upload_bytes,omitempty" yaml:"hosts_upload_bytes" desc:"maximum hosts upload size in bytes"`
	HostsProxyUploadBytes int64 `json:"hosts_proxy_upload_bytes,omitempty" yaml:"hosts_proxy_upload_bytes" desc:"maximum proxied hosts upload size in bytes"`
	HostsStreams          int   `json:"hosts_streams,omitempty" yaml:"hosts_streams" desc:"maximum active file streams per session"`

	// Execution policies: deny first, then operation-covering allow, then default.
	ExecPolicy string   `json:"exec_policy" yaml:"exec_policy" default:"open" desc:"registered command stance: deny | open"`
	ExecDeny   []string `json:"exec_deny" yaml:"exec_deny" desc:"denied registered command names or *"`
	ExecAllow  []string `json:"exec_allow" yaml:"exec_allow" desc:"allowed registered command names or *"`
	FsPolicy   string   `json:"fs_policy" yaml:"fs_policy" default:"deny" desc:"fs default stance: deny (allow only) | open (all except fs_deny)"`
	FsDeny     []string `json:"fs_deny" yaml:"fs_deny" desc:"denied path globs (always deny reads and writes)"`
	FsAllow    []string `json:"fs_allow" yaml:"fs_allow" desc:"allowed paths: read/write by default; ro: prefix grants read only"`
	NetPolicy  string   `json:"net_policy" yaml:"net_policy" default:"open" desc:"sandboxed process outbound stance: open (default) | deny (localhost-only lockdown)"`
	NetDeny    []string `json:"net_deny" yaml:"net_deny" desc:"denied outbound targets host:port (always wins over allow)"`
	NetAllow   []string `json:"net_allow" yaml:"net_allow" desc:"allowed outbound targets host:port (builtin localhost:*; bare host = all ports)"`
	SshPolicy  string   `json:"ssh_policy" yaml:"ssh_policy" default:"deny" desc:"ssh tool target stance: deny | open"`
	SshDeny    []string `json:"ssh_deny" yaml:"ssh_deny" desc:"denied ssh targets host[:port] (always wins over allow)"`
	SshAllow   []string `json:"ssh_allow" yaml:"ssh_allow" desc:"allowed ssh targets host[:port] (bare host = all ports)"`

	// 进程级运行时态（unexported，不参与序列化/落盘）：
	port     int  // 本地管理 API 监听端口（api.Start 监听后 SetPort 写入）
	codeAuto bool // Code 为本次进程随机生成（Save 时跳过落盘）
}

// Port 返回本地管理 API 监听端口（未启动为 0）。
func (o *Options) Port() int { return o.port }

// SetPort 写入本地管理 API 实际监听端口（仅 api.Start 调用）。
func (o *Options) SetPort(p int) { o.port = p }

// newCode 生成校验码（32 hex）。
func newCode() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "" // crypto/rand 不可用时留空（所有 API 调用 401，安全侧失败）
	}
	return hex.EncodeToString(buf)
}

// Global 全局有效配置：NewOptions 初始化 → Load 填充文件值（Code 空则随机生成）→
// flags.AutoRegister(Global) 叠加 flag/env；本地 API 的写操作（bind/set_config）
// 同步更新 Global 并 Save 落盘。
var Global = NewOptions()

// NewOptions 返回带默认值的配置实例（Code 留空，由 Load/LoadFile 生成）。
func NewOptions() *Options {
	return &Options{Host: DefaultHost, ExecTimeout: "30m", HomePath: "/", BrowserWidth: 1280, BrowserHeight: 720, RTC: true}
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

// Normalize 填充缺省值（Host 空 → DefaultHost；HomePath 空/非法 → "/"；
// 授权策略非法值 → deny）。
func (o *Options) Normalize() {
	if strings.TrimSpace(o.Host) == "" {
		o.Host = DefaultHost
	}
	o.HomePath = o.NormalizedHomePath()
	if o.BrowserWidth < 320 || o.BrowserWidth > 4096 {
		o.BrowserWidth = 1280
	}
	if o.BrowserHeight < 320 || o.BrowserHeight > 4096 {
		o.BrowserHeight = 720
	}
	o.ExecPolicy = NormalizePolicy(o.ExecPolicy, PolicyOpen)
	o.FsPolicy = NormalizePolicy(o.FsPolicy, PolicyDeny)
	o.NetPolicy = NormalizePolicy(o.NetPolicy, PolicyOpen)
	o.SshPolicy = NormalizePolicy(o.SshPolicy, PolicyDeny)
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

// HostsURL 由平台地址推导设备管理页入口 {host}/hosts（host 可带产品壳路径前缀，
// 如 http://127.0.0.1:4000/rses/aiv → http://127.0.0.1:4000/rses/aiv/hosts）。
// local_code 由调用方拼接（?local_code={port}.{code}）。
func (o *Options) HostsURL() string {
	h := strings.TrimSpace(o.Host)
	if h == "" {
		h = DefaultHost
	}
	if !strings.Contains(h, "://") {
		h = "https://" + h
	}
	u, err := url.Parse(h)
	if err != nil || u.Host == "" {
		return h
	}
	p := strings.TrimSuffix(u.Path, "/")
	if !strings.HasSuffix(p, "/hosts") {
		p += "/hosts"
	}
	u.Path = p
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
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

// LoadFile 仅读取配置文件返回独立副本（yaml，flags.LoadCfg），不触碰 Global——
// 页面写操作（bind/set_config）落盘用：基于文件配置修改，flag/env 启动覆盖不落盘。
// 文件不存在返回默认配置（非错误），损坏文件由 flags 记 warn 并返回当前值。
// Code 未配置时随机生成（codeAuto=true，Save 不落盘）。
func LoadFile() (*Options, error) {
	o := NewOptions()
	p, err := Path()
	if err != nil {
		return o, err
	}
	data, readErr := os.ReadFile(p)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return o, readErr
	}
	if readErr == nil {
		dec := yaml.NewDecoder(strings.NewReader(string(data)))
		dec.KnownFields(true)
		if err := dec.Decode(o); err != nil {
			return o, err
		}
	}
	if err := o.ValidateAuth(); err != nil {
		return o, err
	}
	o.Normalize()
	if o.Code == "" {
		o.Code = newCode()
		o.codeAuto = true
	}
	return o, nil
}

// Load 读取配置文件填充 Global 并返回（损坏文件不阻断启动，见 LoadFile）。
func Load() (*Options, error) {
	o, err := LoadFile()
	if err == nil {
		Global = o
	}
	return o, err
}

// authMu 守护执行策略十二键的并发读写：api.SetConfig（用户操作）与
// host grant --permanent（AI 经审批）两条写入路径共用。
var authMu sync.RWMutex

// AuthCfg 是四域执行策略快照（policy/deny/allow × fs/exec/net/ssh）。
type AuthCfg struct {
	ExecPolicy string
	ExecDeny   []string
	ExecAllow  []string
	FsPolicy   string
	FsDeny     []string
	FsAllow    []string
	NetPolicy  string
	NetDeny    []string
	NetAllow   []string
	SshPolicy  string
	SshDeny    []string
	SshAllow   []string
}

// AuthSnapshot 返回当前授权配置快照（fsauth/netauth Reconcile 的数据源）。
// policy 在读点归一化（防空值/非法值漂移到安全语义外：fs/ssh 空=deny，net 空=open）。
func AuthSnapshot() AuthCfg {
	authMu.RLock()
	defer authMu.RUnlock()
	return AuthCfg{
		ExecPolicy: NormalizePolicy(Global.ExecPolicy, PolicyOpen), ExecDeny: Global.ExecDeny, ExecAllow: Global.ExecAllow,
		FsPolicy: NormalizePolicy(Global.FsPolicy, PolicyDeny), FsDeny: Global.FsDeny, FsAllow: Global.FsAllow,
		NetPolicy: NormalizePolicy(Global.NetPolicy, PolicyOpen), NetDeny: Global.NetDeny, NetAllow: Global.NetAllow,
		SshPolicy: NormalizePolicy(Global.SshPolicy, PolicyDeny), SshDeny: Global.SshDeny, SshAllow: Global.SshAllow,
	}
}

// SetAuth 更新授权配置（内存即时生效；落盘由调用方负责——
// api.SetConfig 走 Save，grant --permanent 亦同）。
func SetAuth(c AuthCfg) {
	authMu.Lock()
	defer authMu.Unlock()
	Global.ExecPolicy, Global.ExecDeny, Global.ExecAllow = NormalizePolicy(c.ExecPolicy, PolicyOpen), c.ExecDeny, c.ExecAllow
	Global.FsPolicy, Global.FsDeny, Global.FsAllow = NormalizePolicy(c.FsPolicy, PolicyDeny), c.FsDeny, c.FsAllow
	Global.NetPolicy, Global.NetDeny, Global.NetAllow = NormalizePolicy(c.NetPolicy, PolicyOpen), c.NetDeny, c.NetAllow
	Global.SshPolicy, Global.SshDeny, Global.SshAllow = NormalizePolicy(c.SshPolicy, PolicyDeny), c.SshDeny, c.SshAllow
}

// AuthFrom 从 Options 取授权快照（SetAuth 的入参装配）。
func AuthFrom(o *Options) AuthCfg {
	return AuthCfg{
		ExecPolicy: o.ExecPolicy, ExecDeny: o.ExecDeny, ExecAllow: o.ExecAllow,
		FsPolicy: o.FsPolicy, FsDeny: o.FsDeny, FsAllow: o.FsAllow,
		NetPolicy: o.NetPolicy, NetDeny: o.NetDeny, NetAllow: o.NetAllow,
		SshPolicy: o.SshPolicy, SshDeny: o.SshDeny, SshAllow: o.SshAllow,
	}
}

// Save 持久化配置（yaml，flags.DumpCfg 原子写；含凭证，文件权限 0600）。
// 进程随机生成的 Code 不落盘（codeAuto=true 时跳过该字段）。
func Save(o *Options) error {
	if err := o.ValidateAuth(); err != nil {
		return err
	}
	p, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	o.Normalize()
	saveCfg := *o
	if o.codeAuto {
		saveCfg.Code = "" // 自动生成的秘钥不写回（重启换新）
	}
	if err := flags.DumpCfg(p, saveCfg); err != nil {
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
	if err := policy.ValidateFS(o.FsAllow, true); err != nil {
		return err
	}
	if err := policy.ValidateFS(o.FsDeny, false); err != nil {
		return err
	}
	if err := policy.ValidateExec(o.ExecAllow); err != nil {
		return err
	}
	if err := policy.ValidateExec(o.ExecDeny); err != nil {
		return err
	}
	for _, list := range [][]string{o.NetAllow, o.NetDeny, o.SshAllow, o.SshDeny} {
		if err := policy.ValidateEntries(list); err != nil {
			return err
		}
	}
	return nil
}

// LockUpdate serializes local config read-modify-write transactions.
func LockUpdate() func() { configUpdateMu.Lock(); return configUpdateMu.Unlock }

var configUpdateMu sync.Mutex
