// Package host 是 AIC host agent 运行时（docs/instruction_sets_v2.md §6.2）：
// NATS 连接与认证、caps v2 上报、心跳、req 分发（fs/exec）、bg 注册表、
// granted_level 纵深检查（与 vcore 分级表同源）。
//
// 物理 host 命令空间 = 统一命令声明表（§5.1）：恒声明（exec 核心虚拟指令 +
// json + commands + bg_*）+ 启动探测（shell/git，exec.LookPath）+ 本地 provider
// 动态注册（desktop 壳的 browser 等，register.go）。未声明的命令一律拒绝，
// 不存在「未知命令透传」。
package host

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/exec_procs"
	"github.com/veypi/aic-pod/libs/fsauth"
	"github.com/veypi/aic-pod/libs/netauth"
	"github.com/veypi/aic-pod/libs/proto"
	"github.com/veypi/aic-pod/libs/rtc"
	"github.com/veypi/aic-pod/libs/vcore"
)

// Options 客户端配置。
type Options struct {
	Host        string        // 平台地址（如 https://ivec-ai.com，可带路径前缀），NATS 端点据此推断
	Key         string        // "<host_id>.<cred_ver>.<secret>.<uid>"（必填）
	WorkDir     string        // exec/fs 缺省工作区（§2.1.1 workdir 缺省值），默认 /tmp
	DeviceName  string        // 展示名称，默认 hostname
	DeviceType  string        // 客户端类型（cli/browser/...），默认 cli
	Version     string        // 客户端版本号（va.b.c，§6.3 版本门禁）
	ExecTimeout time.Duration // 程序后台自有超时，默认 30m（§5.9）
	NoSandbox   bool          // 全局免沙箱（§5.10）：cfg.Options.NoSandbox 透传
	Code        string        // 本地校验码（caps mgmt 上报；RTC DataChannel 鉴权帧同源）
	RTC         bool          // RTC 直连应答开关（cfg.Options.RTC）：关则不上报 mgmt、不应答信令
	OnLog       func(format string, args ...any)
}

// Client 是 host agent 客户端。
type Client struct {
	execGrantMu sync.RWMutex
	execGrants  map[string][]string
	opts        Options
	nc          *nats.Conn
	kTool       string
	hostID      string
	uid         string
	credVer     uint64
	replay      *replayCache
	cmdsMu      sync.RWMutex                 // cmds/cmdByName：provider 动态注册（register.go）并发保护
	cmds        []proto.CommandDecl          // 统一命令声明表（§5.1：恒声明 + 启动探测 + 壳 provider）
	cmdByName   map[string]proto.CommandDecl // cmds 的 name 索引（路由与纵深检查用）
	procs       *exec_procs.Manager          // exec 子进程统一托管（§5.8/§5.9）
	policy      *fsauth.Policy               // 文件权限模型（fs 域：fs 判定 + 沙箱白名单同实例）
	netPol      *netauth.Policy              // net 域：沙箱内子进程出站目标闸（内建 localhost:*）
	sshPol      *netauth.Policy              // ssh 域：ssh 一级工具目标闸（独立通道，无内建条目）
	rtcSvc      *rtc.Service                 // RTC 直连应答服务（opts.RTC 且 Code 非空时启动）
	logf        func(string, ...any)
}

// New 创建客户端（不连接）。
func New(opts Options) *Client {
	if opts.DeviceType == "" {
		opts.DeviceType = "cli"
	}
	if opts.DeviceName == "" {
		opts.DeviceName, _ = os.Hostname()
	}
	if opts.WorkDir == "" {
		opts.WorkDir = os.TempDir()
	}
	// WorkDir 的反斜杠规范形归一由 proto.ResolvePath 在路径运算层统一处理
	//（Windows 下 os.TempDir() 为反斜杠形，workdir 分支同样归一）。
	if opts.ExecTimeout <= 0 {
		opts.ExecTimeout = 30 * time.Minute
	}
	logf := opts.OnLog
	if logf == nil {
		logf = func(format string, args ...any) {
			fmt.Printf("[%s] %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
		}
	}
	procs := exec_procs.NewManager(opts.ExecTimeout)
	procs.NoSandbox = opts.NoSandbox
	policy := fsauth.New()
	policy.SetWorkDir(opts.WorkDir)
	c := &Client{
		opts:   opts,
		replay: &replayCache{store: map[string]time.Time{}},
		procs:  procs,
		policy: policy,
		netPol: netauth.New(netauth.NetKeys, "localhost:*"),
		sshPol: netauth.New(netauth.SshKeys),
		logf:   logf,
	}
	c.cmds, c.cmdByName = buildCommandTable()
	// cua 探测声明成功 → 建立进程级运行时（MCP 子进程懒启动，cua.go）
	if _, ok := c.cmdByName["cua"]; ok {
		initCuaRuntime(c.logf)
	}
	return c
}

// Connect 连接 NATS，发布 caps v2，订阅会话级 inbox，启动心跳。后台运行，即时返回。
func (c *Client) Connect() error {
	parts := strings.SplitN(c.opts.Key, ".", 4)
	if len(parts) != 4 {
		return fmt.Errorf("invalid credential key")
	}
	c.hostID = parts[0]
	if _, err := fmt.Sscanf(parts[1], "%d", &c.credVer); err != nil || c.credVer == 0 {
		return fmt.Errorf("invalid credential key version")
	}
	secret := parts[2]
	c.uid = parts[3]

	kConnect, _, kTool, err := proto.DeriveKeys(secret, c.hostID)
	if err != nil {
		return fmt.Errorf("derive keys: %w", err)
	}
	c.kTool = kTool

	c.logf("starting aic-host v%s [%s/%s] (host=%s)", c.opts.Version, c.opts.DeviceType, c.opts.DeviceName, c.hostID)

	natsURL := ResolveNATSURL(c.opts.Host)
	opts := []nats.Option{
		nats.Name("aic-host-" + c.hostID),
		nats.TokenHandler(func() string {
			return proto.GenerateConnectToken(c.hostID, c.uid, c.opts.Version, c.opts.DeviceType, c.opts.DeviceName,
				time.Now().UnixMilli(), mustNonce(), kConnect)
		}),
		nats.ReconnectWait(2 * time.Second),
		nats.MaxReconnects(-1),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			c.logf("NATS reconnected, republishing caps")
			c.publishCaps(nc)
		}),
		nats.DisconnectErrHandler(func(nc *nats.Conn, err error) {
			c.logf("NATS disconnected: %v", err)
			c.policy.ResetTemporary()
			c.netPol.ResetTemporary()
			c.sshPol.ResetTemporary()
			c.execGrantMu.Lock()
			c.execGrants = nil
			c.execGrantMu.Unlock()
			if isAuthError(err) {
				c.logf("FATAL: authentication permanently failed — credential expired or revoked. Obtain a new credential and restart.")
				go nc.Close()
			}
		}),
	}
	if strings.HasPrefix(natsURL, "ws") {
		if u, err := parseWSURL(natsURL); err == nil && u.proxyPath != "" {
			opts = append(opts, nats.ProxyPath(u.proxyPath))
			natsURL = u.base
			c.logf("ws proxy path: %s → %s", u.proxyPath, natsURL)
		}
	}

	nc, err := nats.Connect(natsURL, opts...)
	if err != nil {
		return fmt.Errorf("nats connect: %w", err)
	}
	c.nc = nc
	c.logf("connected to NATS: %s", natsURL)

	c.publishCaps(nc)

	inbox, err := proto.HostInboxSubject(c.uid, c.hostID)
	if err != nil {
		return err
	}
	// 每个请求独立 goroutine：避免 handler 阻塞造成 head-of-line 阻塞
	if _, err := nc.Subscribe(inbox, func(msg *nats.Msg) { go c.handleMsg(msg) }); err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	c.logf("listening on %s", inbox)

	go c.heartbeatLoop()

	// RTC 直连应答服务（2026-09-10）：信令走现有通配 inbox，失败不阻断主连接。
	if c.opts.RTC && c.opts.Code != "" {
		if err := c.startRTC(); err != nil {
			c.logf("rtc disabled: %v", err)
		}
	}
	return nil
}

// Close 优雅关闭：关闭 RTC 服务 → 取消订阅 → 断开 NATS。
func (c *Client) Close() error {
	if c.rtcSvc != nil {
		c.rtcSvc.Close()
		c.rtcSvc = nil
	}
	if c.nc != nil {
		c.nc.Close()
		c.nc = nil
	}
	return nil
}

// Reconfigure 应用新运行配置（保存设置后调用）：
// 保留 Client 与 exec_procs Manager（bg 任务原样保留），仅更新
// work_dir/exec_timeout 参数；NATS 地址（host）变化时重连。
// 凭证/身份字段不变（换绑走 bind 流程重建）。
// 注：重连后旧 heartbeatLoop 仍引用 c.nc 继续发 presence（幂等，20s 一次，
// 多一个并发 loop 无功能影响，不额外处理）。
func (c *Client) Reconfigure(o cfg.Options) error {
	opts, err := optionsOf(o, c.opts.DeviceType, c.opts.Version, c.opts.OnLog)
	if err != nil {
		return err
	}
	// 凭证与身份字段保持现有会话不变
	opts.Key = c.opts.Key
	opts.DeviceName = c.opts.DeviceName
	oldURL := ResolveNATSURL(c.opts.Host)
	c.procs.SetExecTimeout(opts.ExecTimeout)
	c.procs.NoSandbox = opts.NoSandbox
	// 授权模型同步（三域）：work_dir 变更 + 配置重载
	//（九键经 cfg.Global 由 api.SetConfig 先行更新）。
	c.policy.SetWorkDir(opts.WorkDir)
	c.syncAuth()
	c.opts = opts
	if ResolveNATSURL(opts.Host) != oldURL {
		if c.nc != nil {
			c.nc.Close()
			c.nc = nil
		}
		return c.Connect()
	}
	return nil
}

// ---- RTC 直连应答（2026-09-10，libs/rtc） ----

// startRTC 启动 RTC 应答服务：信令出向发布到 RtcOutSubject（natsauth host JWT
// pub allow 已放行），fs 执行体为本地控制台信任级。幂等（重连不重复启动——
// UDP mux 与 PeerConnection 生命周期独立于 NATS 连接）。
func (c *Client) startRTC() error {
	if c.rtcSvc != nil {
		return nil
	}
	hostname, _ := os.Hostname()
	svc, err := rtc.New(rtc.Config{
		Code:     c.opts.Code,
		HostID:   c.hostID,
		Hostname: hostname,
		Version:  c.opts.Version,
		Send: func(sig *proto.RtcSignal) {
			if c.nc == nil {
				return
			}
			subj, err := proto.RtcOutSubject(c.uid, c.hostID, c.credVer)
			if err != nil {
				return
			}
			data, _ := json.Marshal(sig)
			c.nc.Publish(subj, data)
		},
		RunFS:    c.runFSLocal,
		ReadBin:  c.readBinLocal,
		WriteBin: c.writeBinLocal,
		Logf:     c.logf,
	})
	if err != nil {
		return err
	}
	c.rtcSvc = svc
	return nil
}

// handleRTCSignal 处理一条 rtc.in 信令（dispatch.go handleMsg 路由过来）。
func (c *Client) handleRTCSignal(data []byte) {
	if c.rtcSvc == nil {
		return
	}
	var sig proto.RtcSignal
	if err := json.Unmarshal(data, &sig); err != nil {
		return
	}
	c.rtcSvc.HandleSignal(&sig)
}

// runFSLocal 是 RTC 直连通道的 fs 执行体：code 鉴权通过 = 本地控制台信任级
// （granted=9，不再出现审批；fsauth 三域 deny/allow 照常生效，deny 恒拒不可绕过）。
// sid 为空：无会话临时 grant 视图（与 run_tool 直发同语义）。
func (c *Client) runFSLocal(ctx context.Context, raw json.RawMessage) (*vcore.Result, error) {
	env := c.newEnv("", "")
	env.Granted = 9
	return vcore.RunFS(ctx, env, raw)
}

// readBinLocal 是 RTC 直连通道 readbin op 的执行体（2026-09-10，预览/下载
// 大二进制的字节出口）：与 runFSLocal 同信任级、同 fsauth 判定实例。
func (c *Client) readBinLocal(path string, off, length int64) ([]byte, string, int64, error) {
	env := c.newEnv("", "")
	env.Granted = 9
	return vcore.ReadBin(env, path, off, length)
}

// writeBinLocal 是 RTC 直连通道 writebin op 的执行体（2026-09-12，写方向的
// 原始字节入口，host fs put 二进制内容用）：与 runFSLocal 同信任级、同 fsauth
// 判定实例。
func (c *Client) writeBinLocal(path string, data []byte) (int, error) {
	env := c.newEnv("", "")
	env.Granted = 9
	return vcore.WriteBin(env, path, data)
}

// ---- caps v2 上报（§6.3） ----

// buildCommandTable 构建物理 host 的统一命令声明表（§5.1）：
//   - 恒声明：exec 核心虚拟指令（curl）+ json + commands + bg_list/bg_wait/bg_kill
//     （vcore 元数据同源）；文件类指令属 fs 指令集（fs.actions 声明）
//   - 启动探测（exec.LookPath，探测到才声明）：
//     shell（bash/zsh/sh/fish；Windows: powershell/pwsh/cmd）→ level 3（逃生舱）；
//     git → level 1（本地凭证天然可用）；ssh/scp → level 3（目标闸独立通道）
//
// browser 等壳能力不在此探测——由壳进程经本地 provider 通道动态注册（register.go，
// desktop/浏览器插件各自实现，agent-browser CLI 依赖已彻底移除）。
func buildCommandTable() ([]proto.CommandDecl, map[string]proto.CommandDecl) {
	var cmds []proto.CommandDecl
	seen := map[string]bool{}
	add := func(d proto.CommandDecl) {
		if seen[d.Name] {
			return
		}
		seen[d.Name] = true
		cmds = append(cmds, d)
	}
	for _, name := range vcore.CoreCommandNames() {
		if d, ok := vcore.Decl(name); ok {
			add(d)
		}
	}
	for _, name := range []string{"commands", "json", "bg_list", "bg_wait", "bg_kill", "grant"} {
		if d, ok := vcore.Decl(name); ok {
			add(d)
		}
	}
	// 启动探测：未安装的命令不声明（AI 经 commands 自然发现不可用）
	for _, sh := range []string{"bash", "zsh", "sh", "fish", "powershell", "pwsh", "cmd"} {
		if _, err := exec.LookPath(sh); err == nil {
			add(proto.CommandDecl{
				Name: sh, Desc: "run shell commands (escape hatch)",
				Help:          sh + " -c \"<command>\"\n  run arbitrary shell commands with full host semantics (escape hatch)",
				RequiredLevel: proto.LevelDanger,
			})
		}
	}
	if _, err := exec.LookPath("git"); err == nil {
		if d, ok := vcore.Decl("git"); ok {
			add(d)
		}
	}
	// ssh 一级工具：ssh 二进制存在才声明（目标闸在 ssh 域 Policy，独立通道）
	if _, err := exec.LookPath("ssh"); err == nil {
		if d, ok := vcore.Decl("ssh"); ok {
			add(d)
		}
	}
	// scp 一级工具：scp 二进制存在才声明（目标闸同 ssh 域，本地侧 fsauth 门控）
	if _, err := exec.LookPath("scp"); err == nil {
		if d, ok := vcore.Decl("scp"); ok {
			add(d)
		}
	}
	// cua 一级命令（§5.10）：cua-driver 二进制探测（CUA_DRIVER_PATH → PATH →
	// 常见安装路径），探测到才声明（运行时单例在 New() 建立，需 logf）
	if findCuaDriver() != "" {
		if d, ok := vcore.Decl("cua"); ok {
			add(d)
		}
	}
	// 壳 provider（desktop 的 browser 等）：进程级注册表汇入（register.go）
	for _, d := range providerDecls() {
		add(d)
	}
	byName := make(map[string]proto.CommandDecl, len(cmds))
	for _, d := range cmds {
		byName[d.Name] = d
	}
	return cmds, byName
}

// buildCaps 构造物理 host 的 caps v2（§6.3）：
// fs.actions=null（全部 8 个）；exec.commands = 统一命令声明表。
func (c *Client) buildCaps() *proto.Caps {
	hostname, _ := os.Hostname()
	c.cmdsMu.RLock()
	decls := make([]proto.CommandDecl, len(c.cmds))
	copy(decls, c.cmds)
	c.cmdsMu.RUnlock()
	return &proto.Caps{
		HostID:        c.hostID,
		CredentialVer: c.credVer,
		AgentVersion:  c.opts.Version,
		DeviceType:    c.opts.DeviceType,
		Hostname:      hostname,
		DeviceInfo:    deviceInfo(),
		Mgmt:          c.buildMgmt(),
		FS:            proto.FSCaps{},                  // actions=null = 全部 8 个
		Exec:          proto.ExecCaps{Commands: decls}, // 统一命令声明表
	}
}

// buildMgmt 构造本地管理面声明：仅 RTC 开关开启且持有校验码时上报
// （服务端以 mgmt 存在性判定设备直连能力，页面据此发起 RTC 直连）。
func (c *Client) buildMgmt() *proto.MgmtCaps {
	if !c.opts.RTC || c.opts.Code == "" {
		return nil
	}
	return &proto.MgmtCaps{Code: c.opts.Code, RTC: true}
}

func (c *Client) publishCaps(nc *nats.Conn) {
	subj, err := proto.CapsSubject(c.uid, c.hostID, c.credVer)
	if err != nil {
		c.logf("caps subject: %v", err)
		return
	}
	data, _ := json.Marshal(c.buildCaps())
	nc.Publish(subj, data)
	c.cmdsMu.RLock()
	n := len(c.cmds)
	c.cmdsMu.RUnlock()
	c.logf("caps published to %s (%d commands)", subj, n)
}

func (c *Client) heartbeatLoop() {
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if c.nc == nil {
			return
		}
		subj, err := proto.PresenceSubject(c.uid, c.hostID, c.credVer)
		if err != nil {
			continue
		}
		presence := map[string]any{
			"host_id":        c.hostID,
			"credential_ver": c.credVer,
			"running":        1,
			"sent_at":        time.Now().UTC().Format(time.RFC3339),
		}
		data, _ := json.Marshal(presence)
		c.nc.Publish(subj, data)
	}
}

func isAuthError(err error) bool {
	// 正常关闭（nc.Close()）时 DisconnectErrHandler 的 err 为 nil，直接判否。
	if err == nil {
		return false
	}
	// nats.go 报 "Authentication Violation"/"Authorization Violation"（首字母大写），
	// 统一小写后匹配，避免致命认证分支永不命中。
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "authentication") || strings.Contains(s, "authorization")
}
