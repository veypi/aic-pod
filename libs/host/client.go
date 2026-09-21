// Package host 是 AIC host agent 运行时（docs/instruction_sets_v2.md §6.2）：
// NATS 连接与认证、能力上报、心跳、fs/exec 方法分发、执行管理器装配、
// granted_level 纵深检查（与 vcore 分级表同源）。
//
// 物理 host 命令空间 = 统一命令声明表（§5.1）：恒声明（exec 核心虚拟指令 +
// json + commands + bg_*）+ 启动探测（shell/git，exec.LookPath）。browser/cua
// 一并注册为 exec.commands。未声明的命令一律拒绝，
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
	"github.com/veypi/aic-pod/libs/browser"
	"github.com/veypi/aic-pod/libs/exec_procs"
	"github.com/veypi/aic-pod/libs/fsauth"
	"github.com/veypi/aic-pod/libs/hostauth"
	"github.com/veypi/aic-pod/libs/hostfs"
	tool "github.com/veypi/aic-pod/libs/hosts_tool"
	"github.com/veypi/aic-pod/libs/netauth"
	"github.com/veypi/aic-pod/libs/proto"
	"github.com/veypi/aic-pod/libs/rtc"
	"github.com/veypi/aic-pod/libs/vcore"

	natswire "github.com/veypi/aic-pod/protocol/hosts_nats"
	rtcwire "github.com/veypi/aic-pod/protocol/hosts_rtc"
	toolwire "github.com/veypi/aic-pod/protocol/hosts_tools"
)

// Options 客户端配置。
type Options struct {
	BrowserPath, BrowserStateDir string
	BrowserWidth, BrowserHeight  int
	Transfers                    hostfs.TransferConfig
	Host                         string        // 平台地址（如 https://ivec-ai.com，可带路径前缀），NATS 端点据此推断
	Key                          string        // "<host_id>.<cred_ver>.<secret>.<uid>"（必填）
	WorkDir                      string        // exec/fs 缺省工作区（§2.1.1 workdir 缺省值），默认 /tmp
	DeviceName                   string        // 展示名称，默认 hostname
	DeviceType                   string        // 客户端类型（cli/desktop/...），默认 cli
	Version                      string        // 客户端版本号（va.b.c，§6.3 版本门禁）
	ExecTimeout                  time.Duration // 程序后台自有超时，默认 30m（§5.9）
	NoSandbox                    bool          // 全局免沙箱（§5.10）：cfg.Options.NoSandbox 透传
	RTC                          bool          // RTC 直连应答开关（cfg.Options.RTC）：关闭仍保留文件 proxy
	OnLog                        func(format string, args ...any)
}

// Client 是 host agent 客户端。
type Client struct {
	sessionRoot string
	tools       *tool.Dispatcher
	browser     *browser.Service

	execGrantMu sync.RWMutex
	execGrants  map[string][]string
	optsMu      sync.RWMutex
	opts        Options
	nc          *nats.Conn
	kTool       string
	hostID      string
	uid         string
	credVer     uint64
	replay      *replayCache
	procs       *exec_procs.Manager // exec 子进程统一托管（§5.8/§5.9）
	policy      *fsauth.Policy      // 文件权限模型（fs 域：fs 判定 + 沙箱白名单同实例）
	netPol      *netauth.Policy     // net 域：沙箱内子进程出站目标闸（内建 localhost:*）
	sshPol      *netauth.Policy     // ssh 域：ssh 一级工具目标闸（独立通道，无内建条目）
	rtcMu       sync.RWMutex
	access      *hostauth.Access
	files       *hostfs.FS
	bytes       *hostfs.Bytes
	initErr     error
	rtcSvc      *rtc.Service // hosts_rtc/1 直连服务
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
	procs.SetNoSandbox(opts.NoSandbox)
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

	if parts := strings.SplitN(opts.Key, ".", 4); len(parts) == 4 {
		c.hostID = parts[0]
		c.uid = parts[3]
		_, _, c.kTool, _ = proto.DeriveKeys(parts[2], parts[0])
		_, _ = fmt.Sscanf(parts[1], "%d", &c.credVer)
	}
	c.initTools()
	return c
}

// Connect 连接 NATS，发布 caps v2，订阅会话级 inbox，启动心跳。后台运行，即时返回。
func (c *Client) Connect() error {
	if c.initErr != nil {
		return c.initErr
	}
	parts := strings.SplitN(c.options().Key, ".", 4)
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

	c.logf("starting aic-host v%s [%s/%s] (host=%s)", c.options().Version, c.options().DeviceType, c.options().DeviceName, c.hostID)

	natsURL := ResolveNATSURL(c.options().Host)
	opts := []nats.Option{
		nats.Name("aic-host-" + c.hostID),
		nats.TokenHandler(func() string {
			return proto.GenerateConnectToken(c.hostID, c.uid, c.options().Version, c.options().DeviceType, c.options().DeviceName,
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
			if isAuthError(err) {
				c.logf("FATAL: authentication permanently failed — credential expired or revoked. Obtain a new credential and restart.")
				go func() { c.stopRTC(); nc.Close() }()
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

	// The command runtime also serves authenticated server proxy when RTC is off.
	if err := c.startCommands(); err != nil {
		c.logf("device commands unavailable: %v", err)
	}
	if c.options().RTC {
		if err := c.startRTC(); err != nil {
			c.logf("rtc disabled: %v", err)
		}
	}
	c.publishCaps(nc)
	return nil
}

// Close 优雅关闭：关闭 RTC 服务 → 取消订阅 → 断开 NATS。
func (c *Client) Close() error {
	shutdown, cancelRuns := context.WithTimeout(context.Background(), 5*time.Second)
	_ = c.procs.Close(shutdown)
	cancelRuns()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = c.tools.Close(ctx)
	}()
	if c.files != nil {
		_ = c.files.Close()
	}
	if c.bytes != nil {
		_ = c.bytes.Close()
	}
	service := c.detachRTC()
	if service != nil {
		service.RevokeAll()
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
	opts, err := optionsOf(o, c.options().DeviceType, c.options().Version, c.options().OnLog)
	if err != nil {
		return err
	}
	// 凭证与身份字段保持现有会话不变
	opts.Key = c.options().Key
	opts.DeviceName = c.options().DeviceName
	oldURL := ResolveNATSURL(c.options().Host)
	restartRTC := c.options().WorkDir != opts.WorkDir || c.options().RTC != opts.RTC || c.options().Transfers != opts.Transfers
	if restartRTC {
		c.stopRTC()
	}
	c.procs.SetExecTimeout(opts.ExecTimeout)
	c.procs.SetNoSandbox(opts.NoSandbox)
	// 授权模型同步（三域）：work_dir 变更 + 配置重载
	//（九键经 cfg.Global 由 api.SetConfig 先行更新）。
	c.policy.SetWorkDir(opts.WorkDir)
	c.syncAuth()
	c.optsMu.Lock()
	c.opts = opts
	c.optsMu.Unlock()
	if c.files != nil {
		_, home, err := deviceFileRoots(opts.WorkDir)
		if err != nil {
			return err
		}
		c.files.Configure(home, opts.Transfers.ProxyUploadBytes)
		c.bytes.Configure(opts.Transfers.MaxUploadBytes, opts.Transfers.MaxSources)
	}
	if c.browser != nil {
		c.browser.Configure(opts.BrowserPath, opts.BrowserWidth, opts.BrowserHeight)
	}
	if restartRTC && c.nc != nil {
		if err := c.startCommands(); err != nil {
			return err
		}
		if opts.RTC {
			if err := c.startRTC(); err != nil {
				return err
			}
		}
	}
	if c.nc != nil {
		c.publishCaps(c.nc)
	}
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
// pub allow 已放行），设备命令使用独立签名票据。幂等（重连不重复启动——
// UDP mux 与 PeerConnection 生命周期独立于 NATS 连接）。
func (c *Client) startCommands() error {
	c.rtcMu.Lock()
	defer c.rtcMu.Unlock()
	if c.access != nil {
		return nil
	}
	commands, err := c.newAccess()
	if err != nil {
		return err
	}
	c.access = commands
	return nil
}

func (c *Client) startRTC() error {
	if err := c.startCommands(); err != nil {
		return err
	}
	c.rtcMu.Lock()
	defer c.rtcMu.Unlock()
	if c.rtcSvc != nil {
		return nil
	}
	commands := c.access
	hostname, _ := os.Hostname()
	svc, err := rtc.New(rtc.Config{
		Authorization: commands,
		Tools:         c,
		HostID:        c.hostID,
		Hostname:      hostname,
		Version:       c.options().Version,
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
		Logf: c.logf,
	})
	if err != nil {
		return err
	}
	c.access = commands
	c.rtcSvc = svc
	return nil
}

// handleRTCSignal 处理一条 rtc.in 信令（dispatch.go handleMsg 路由过来）。
func (c *Client) handleRTCSignal(data []byte) {
	c.rtcMu.RLock()
	svc := c.rtcSvc
	c.rtcMu.RUnlock()
	if svc == nil {
		return
	}
	var sig proto.RtcSignal
	if err := json.Unmarshal(data, &sig); err != nil {
		return
	}
	svc.HandleSignal(&sig)
}

// ---- caps v2 上报（§6.3） ----

// commandDefinitions builds raw-argv declarations before registering them alongside service commands:
//   - 恒声明：exec 核心虚拟指令（curl）+ json + commands + bg_list/bg_wait/bg_kill
//     （vcore 元数据同源）；文件类指令属 fs 指令集（fs.actions 声明）
//   - 启动探测（exec.LookPath，探测到才声明）：
//     shell（bash/zsh/sh/fish；Windows: powershell/pwsh/cmd）→ level 3（逃生舱）；
//     git → level 1（本地凭证天然可用）；ssh/scp → level 3（目标闸独立通道）
//
// browser/cua 由 initTools 一次声明到 hosts_tool，不进入普通进程命令表。
func commandDefinitions() []proto.CommandDecl {
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
	return cmds
}

// buildCaps 构造物理 host 的 caps v2（§6.3）：
// FS 与 exec 元数据均从实际注册声明生成。
func (c *Client) buildCaps() *proto.Caps {
	hostname, _ := os.Hostname()
	decls := c.tools.Commands(context.Background(), tool.Caller{Subject: "catalog", ConnectionID: "catalog", Level: 9, ExpiresAt: time.Now().Add(time.Minute)})
	return &proto.Caps{
		HostID:        c.hostID,
		CredentialVer: c.credVer,
		AgentVersion:  c.options().Version,
		DeviceType:    c.options().DeviceType,
		Hostname:      hostname,
		DeviceInfo:    deviceInfo(),
		Mgmt:          c.buildMgmt(),

		ToolProtocols: []string{toolwire.Protocol, natswire.Protocol, rtcwire.Protocol},
		FS:            c.filesystemCaps(),                                      // 内建 FS 方法与 AI 文本动作
		Exec:          proto.ExecCaps{Epoch: c.procs.Epoch(), Commands: decls}, // 统一命令声明表
	}
}

// buildMgmt advertises only the live generic transport, never a management code.
func (c *Client) buildMgmt() *proto.MgmtCaps {
	c.rtcMu.RLock()
	defer c.rtcMu.RUnlock()
	if c.files == nil {
		return nil
	}
	m := &proto.MgmtCaps{Transports: map[string]proto.TransportCaps{"proxy": {Enabled: true, Protocol: natswire.Protocol, Commands: []string{"fs"}}}}
	if c.rtcSvc != nil {
		commands := []string{"fs", "exec"}

		m.Transports["rtc"] = proto.TransportCaps{Enabled: true, Protocol: rtcwire.Protocol, Commands: commands}
	}
	return m
}
func (c *Client) detachRTC() *hostauth.Access {
	c.rtcMu.Lock()
	svc, commands := c.rtcSvc, c.access
	c.rtcSvc, c.access = nil, nil
	c.rtcMu.Unlock()
	if svc != nil {
		svc.Close()
	}
	return commands
}
func (c *Client) stopRTC() {
	if service := c.detachRTC(); service != nil {
		service.RevokeAll()
	}
}

func (c *Client) publishCaps(nc *nats.Conn) {
	subj, err := proto.CapsSubject(c.uid, c.hostID, c.credVer)
	if err != nil {
		c.logf("caps subject: %v", err)
		return
	}
	data, _ := json.Marshal(c.buildCaps())
	nc.Publish(subj, data)
	n := len(c.tools.Commands(context.Background(), tool.Caller{Subject: "catalog", ConnectionID: "catalog", Level: 9, ExpiresAt: time.Now().Add(time.Minute)}))
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

func (c *Client) filesystemCaps() proto.FSCaps {
	catalog := c.tools.Catalog(context.Background(), tool.Caller{Subject: "catalog", ConnectionID: "catalog", Level: 9, ExpiresAt: time.Now().Add(time.Minute)})
	actions := []string{}
	for _, m := range catalog.FS {
		if strings.HasPrefix(m.Name, "text.") {
			actions = append(actions, strings.TrimPrefix(m.Name, "text."))
		}
	}
	return proto.FSCaps{Actions: &actions, Methods: catalog.FS}
}

func (c *Client) options() Options { c.optsMu.RLock(); defer c.optsMu.RUnlock(); return c.opts }
