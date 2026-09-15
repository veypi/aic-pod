// Copyright (C) 2025 veypi <i@veypi.com>
// Distributed under terms of the MIT license.

// Package cfg 是 aic-pod 的配置中心（cli/desktop 共用同一份配置）：
// Options 结构体 + Global 全局有效配置，落盘 UserConfigDir/aic/config.yaml。
package cfg

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"

	"github.com/rs/zerolog"
	"github.com/veypi/vigo/flags"
	"gopkg.in/natefinch/lumberjack.v2"
	"gopkg.in/yaml.v3"
)

// DefaultHost 是默认平台地址。
const DefaultHost = "https://ivec-ai.com"

// Version 客户端版本：Makefile -X github.com/veypi/aic-pod/cfg.Version 注入 git
// 版本，未注入时以此兑底。发版只改本变量与 browser/manifest.json（无 v 前缀）。
var Version = "v0.6.5"

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
	// 并在 caps 的 mgmt 字段上报 Code 供服务端缓存、owner 页面鉴权使用；
	// 关闭则不上报 mgmt、不应答 rtc.in 信令。
	RTC bool `json:"rtc" yaml:"rtc" default:"true" desc:"answer WebRTC direct links from owner pages (default true)"`

	// 三域授权模型（fs/net/ssh × policy/deny/allow）。统一判定式：
	// deny 命中 → 拒，除非存在更具体的 allow（具体度优先，同精度 deny 胜）；
	// policy=open → 未命中 deny 一律放；policy=deny → 仅 allow 放行。
	//   - fs：读默认开（deny 除外）；写 policy=deny 时仅内建可写根（工作区/临时区/
	//     会话区/缓存/公共区）+ fs_allow + 临时 grant，policy=open 时除 fs_deny 全可写。
	//     **fs_allow 显式条目压过 fs_deny**（allow 覆盖 deny）：裸路径覆盖其子树，
	//     带通配条目按 glob 精确匹配（如 `**/.env` 只放 .env）；内建便利根与临时
	//     grant 不压（它们是写便利不是信任声明——公共区的 browser cookie 库、
	//     缓存/工作区里的 .env/*.pem/*.key 仍受保护）；deny 恒为读写双拒且不可审批
	//   - net：exec 沙箱内子进程出站（vcore curl 经 shell curl 同样进沙箱）。
	//     默认 open（2026-09-07 用户定：deny 默认会让 apt/wget/git clone 等工具链全断，
	//     deny 作锁定模式选用——内核层只支持 localhost-only 粗粒度（darwin seatbelt
	//     实测 host 必须为 */localhost；linux bwrap 全有/全无），deny 模式下非
	//     loopback 白名单条目内核按 *:port 粗放行 + curl 虚拟指令工具层按 host:port
	//     精判）；内建默认 allow localhost:*（具体端口 net_deny 可反杀——具体度优先）
	//   - ssh：ssh 一级工具目标闸（独立通道：ssh 免沙箱执行，net 规则不作用于它）
	// 具体度：host 恒精确匹配（不支持通配主机），端口数字 > *；同精度 deny 胜。
	// 内存即时生效（fsauth/netauth 每次判定/每次沙箱 Start 读当前值），
	// set_config 与 grant <域> <目标> --permanent 动态改（落点即对应 allow 列表）。
	FsPolicy  string   `json:"fs_policy" yaml:"fs_policy" default:"deny" desc:"fs write default stance: deny (builtin roots + fs_allow only) | open (all except fs_deny)"`
	FsDeny    []string `json:"fs_deny" yaml:"fs_deny" desc:"denied path globs (no read/write, not approval-able; overridden by explicit fs_allow entries)"`
	FsAllow   []string `json:"fs_allow" yaml:"fs_allow" desc:"explicit allow entries: writable + deny override — bare path covers its subtree, glob entry matches exactly (e.g. **/.env); overrides fs_deny"`
	NetPolicy string   `json:"net_policy" yaml:"net_policy" default:"open" desc:"sandboxed process outbound stance: open (default) | deny (localhost-only lockdown)"`
	NetDeny   []string `json:"net_deny" yaml:"net_deny" desc:"denied outbound targets host:port (always wins over allow)"`
	NetAllow  []string `json:"net_allow" yaml:"net_allow" desc:"allowed outbound targets host:port (builtin localhost:*; bare host = all ports)"`
	SshPolicy string   `json:"ssh_policy" yaml:"ssh_policy" default:"deny" desc:"ssh tool target stance: deny | open"`
	SshDeny   []string `json:"ssh_deny" yaml:"ssh_deny" desc:"denied ssh targets host[:port] (always wins over allow)"`
	SshAllow  []string `json:"ssh_allow" yaml:"ssh_allow" desc:"allowed ssh targets host[:port] (bare host = all ports)"`

	// 进程级运行时态（unexported，不参与序列化/落盘）：
	port     int  // 本地管理 API 监听端口（api.Start 监听后 SetPort 写入）
	codeAuto bool // Code 为本次进程随机生成（Save 时跳过落盘）
}

// UnmarshalYAML 接受两种键形态：yaml tag（snake_case，与 json tag/本地 API 同名，
// 文档与 Save 落盘形态）与结构体默认键（小写字段名，2026-09-09 之前的落盘形态）。
// 同名字段以 yaml tag 形态优先（显式键胜出）；两种形态都兼容意味着历史配置文件
// （fspolicy/fsdeny/workdir 等）不会因格式切换而失效，Save 后自愈为新形态。
// 坏文件/未知键由 yaml 层忽略（LoadFile 仅 warn，不阻断启动）。
func (o *Options) UnmarshalYAML(value *yaml.Node) error {
	type alias Options // 别名不继承本方法，避免递归
	var a alias
	if err := value.Decode(&a); err != nil {
		return err
	}
	*o = Options(a)
	var raw map[string]yaml.Node
	if err := value.Decode(&raw); err != nil {
		return err
	}
	rv := reflect.ValueOf(o).Elem()
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := strings.Split(f.Tag.Get("yaml"), ",")[0]
		if tag == "" || tag == "-" {
			continue
		}
		if _, ok := raw[tag]; ok {
			continue // snake_case 形态已生效
		}
		node, ok := raw[strings.ToLower(f.Name)]
		if !ok {
			continue
		}
		if err := node.Decode(rv.Field(i).Addr().Interface()); err != nil {
			return err
		}
	}
	return nil
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
	return &Options{Host: DefaultHost, ExecTimeout: "30m", HomePath: "/", RTC: true}
}

// 授权策略取值（fs_policy/net_policy/ssh_policy 的合法值）。
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
	flags.LoadCfg(p, o)
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
	Global = o
	return o, err
}

// authMu 守护授权九键的并发读写：api.SetConfig（用户操作）与
// host grant --permanent（AI 经审批）两条写入路径共用。
var authMu sync.RWMutex

// AuthCfg 是三域授权配置快照（policy/deny/allow × fs/net/ssh）。
type AuthCfg struct {
	FsPolicy  string
	FsDeny    []string
	FsAllow   []string
	NetPolicy string
	NetDeny   []string
	NetAllow  []string
	SshPolicy string
	SshDeny   []string
	SshAllow  []string
}

// AuthSnapshot 返回当前授权配置快照（fsauth/netauth Reconcile 的数据源）。
// policy 在读点归一化（防空值/非法值漂移到安全语义外：fs/ssh 空=deny，net 空=open）。
func AuthSnapshot() AuthCfg {
	authMu.RLock()
	defer authMu.RUnlock()
	return AuthCfg{
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
	Global.FsPolicy, Global.FsDeny, Global.FsAllow = NormalizePolicy(c.FsPolicy, PolicyDeny), c.FsDeny, c.FsAllow
	Global.NetPolicy, Global.NetDeny, Global.NetAllow = NormalizePolicy(c.NetPolicy, PolicyOpen), c.NetDeny, c.NetAllow
	Global.SshPolicy, Global.SshDeny, Global.SshAllow = NormalizePolicy(c.SshPolicy, PolicyDeny), c.SshDeny, c.SshAllow
}

// AuthFrom 从 Options 取授权快照（SetAuth 的入参装配）。
func AuthFrom(o *Options) AuthCfg {
	return AuthCfg{
		FsPolicy: o.FsPolicy, FsDeny: o.FsDeny, FsAllow: o.FsAllow,
		NetPolicy: o.NetPolicy, NetDeny: o.NetDeny, NetAllow: o.NetAllow,
		SshPolicy: o.SshPolicy, SshDeny: o.SshDeny, SshAllow: o.SshAllow,
	}
}

// Save 持久化配置（yaml，flags.DumpCfg 原子写；含凭证，文件权限 0600）。
// 进程随机生成的 Code 不落盘（codeAuto=true 时跳过该字段）。
func Save(o *Options) error {
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
