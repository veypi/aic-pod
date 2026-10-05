// Package host 是 AIC host agent 运行时（hosts-vsh-redesign）：
// NATS 连接与认证、能力上报、心跳、请求分发、执行管理器装配。
//
// 物理 host 命令空间（vsh 引擎化）：exec 唯一执行动作（script 契约）——
// 内建 90 + jq + 平台命令（commands/bg/grant）由引擎 Registry
// 收口，browser 自 v6 P5 起是已装 skill 包（aic-skills/browser）不再是
// 内建；原生命令按 exec_rules 首命中判定；
// 白名单外一律 127，不存在「未知命令透传」。审批只留 grant/nosandbox 两处
// 且全在发送前；pod 不重新分类审批，只在执行点看 rules。
package host

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/veypi/aic-pod/protocol"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/fsauth"
	"github.com/veypi/aic-pod/libs/fsx"
	"github.com/veypi/aic-pod/libs/hostauth"
	"github.com/veypi/aic-pod/libs/hostfs"
	"github.com/veypi/aic-pod/libs/mcpx"
	"github.com/veypi/aic-pod/libs/netauth"

	"github.com/veypi/aic-pod/libs/rtc"
	"github.com/veypi/vbox"
)

// Options 客户端配置。
type Options struct {
	MCP         mcpx.Settings
	Transfers   hostfs.TransferConfig
	Host        string        // 平台地址（如 https://ivec-ai.com，可带路径前缀），NATS 端点据此推断
	Key         string        // "<host_id>.<cred_ver>.<secret>.<uid>"（必填）
	WorkDir     string        // exec/fs 缺省工作区（§2.1.1 workdir 缺省值），默认 /tmp
	DeviceName  string        // 展示名称，默认 hostname
	DeviceType  string        // 客户端类型（cli/desktop/...），默认 cli
	Version     string        // 客户端版本号（va.b.c，§6.3 版本门禁）
	ExecTimeout time.Duration // 程序后台自有超时，默认 30m（§5.9）
	NoSandbox   bool          // 全局免沙箱（§5.10）：cfg.Options.NoSandbox 透传
	RTC         bool          // RTC 直连应答开关（cfg.Options.RTC）：关闭仍保留文件 proxy
	OnLog       func(format string, args ...any)
}

// Client 是 host agent 客户端。
type Client struct {
	sessionRoot    string
	mcpMu          sync.RWMutex
	mcpServices    *mcpx.Manager
	mcpAuth        cfg.AuthCfg
	browserConfig  *mcpx.Config
	browserContext context.Context
	browserCancel  context.CancelFunc

	execMu      sync.Mutex
	execHandles map[string]*execHandleEntry // 当前执行请求登记表（前台执行）

	execGrantMu     sync.RWMutex
	execGrants      map[string][]string
	optsMu          sync.RWMutex
	opts            Options
	lifecycleMu     sync.Mutex // serializes Connect, Reconfigure and Close
	closed          bool
	ncMu            sync.RWMutex
	nc              *nats.Conn
	heartbeatCancel context.CancelFunc
	heartbeatDone   chan struct{}
	kTool           string
	hostID          string
	uid             string
	credVer         uint64
	replay          *replayCache
	procs           *vbox.Manager    // exec 子进程统一托管（§5.8/§5.9）
	policy          *fsauth.Policy   // 文件权限模型（fs 域：fs 判定 + 沙箱白名单同实例）
	vsh             vshState         // vsh 引擎装配态（script 执行，惰性构建）
	netPol          *netauth.Policy  // net 域：沙箱内子进程出站目标闸（内建 localhost:*）
	sshPol          *netauth.Policy  // ssh 域：ssh 一级工具目标闸（独立通道，无内建条目）
	sshRun          sshProcessRunner // nil uses the managed native process runner
	rtcMu           sync.RWMutex
	access          *hostauth.Access
	files           *hostfs.FS
	bytes           *hostfs.Bytes
	initErr         error
	rtcSvc          *rtc.Service // hosts_rtc/2 直连服务
	logf            func(string, ...any)
}

// defaultWorkDir 缺省工作区 = ~/aic（2026-09-28 拍板；历史缺省为系统临时
// 目录——临时目录语义随机器清理漂移，不适合作为默认工作区）。UserHomeDir
// 失败时退系统临时目录。
func defaultWorkDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, "aic")
	}
	return os.TempDir()
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
		opts.WorkDir = defaultWorkDir()
	}
	// WorkDir 的反斜杠规范形归一由 protocol.ResolvePath 在路径运算层统一处理
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
	procs := vbox.NewManager()
	procs.SetNoSandbox(opts.NoSandbox)
	procs.SetLogf(logf)
	// 配置与便利根由 fsauth 组合，启动进程时只下发权限快照。
	policy, fsErr := fsauth.New()
	netPolicy, netErr := netauth.New(netauth.NetKeys, "localhost:*")
	sshPolicy, sshErr := netauth.New(netauth.SshKeys)
	policy.SetWorkDir(opts.WorkDir)
	c := &Client{
		opts:        opts,
		replay:      &replayCache{store: map[string]time.Time{}},
		procs:       procs,
		policy:      policy,
		netPol:      netPolicy,
		sshPol:      sshPolicy,
		execGrants:  map[string][]string{},
		execHandles: map[string]*execHandleEntry{},
		logf:        logf,
	}

	if parts := strings.SplitN(opts.Key, ".", 4); len(parts) == 4 {
		c.hostID = parts[0]
		c.uid = parts[3]
		_, _, c.kTool, _ = protocol.DeriveKeys(parts[2], parts[0])
		_, _ = fmt.Sscanf(parts[1], "%d", &c.credVer)
	}
	// 会话区根 = {StateDir}/sessions：与 fsauth 会话便利根
	// 同路径（生产此前从不赋值 → sessionWorkDir 退 Temp 兜底，「会话区」分裂
	// 为两个概念；win 沙箱对 fsauth 会话根行 grantDirWrite 因目录从未存在而
	// fail-closed）。ensureSessionWorkDir 自此创建真实目录，两侧归一。
	if dir, err := cfg.StateDir(); err == nil {
		c.sessionRoot = filepath.Join(dir, "sessions")
	}
	if fsErr != nil {
		c.initErr = fsErr
	} else if netErr != nil {
		c.initErr = netErr
	} else if sshErr != nil {
		c.initErr = sshErr
	}
	if c.initErr == nil {
		c.initTools()
	}
	return c
}

// Connect 连接 NATS，发布 caps v2，订阅会话级 inbox，启动心跳。后台运行，即时返回。
func (c *Client) Connect() error {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	return c.connect()
}

func (c *Client) connect() error {
	if c.closed {
		return fmt.Errorf("host client closed")
	}
	if nc := c.connection(); nc != nil && !nc.IsClosed() {
		return nil
	}
	c.closeConnection()
	if c.initErr != nil {
		return c.initErr
	}
	parts := strings.SplitN(c.options().Key, ".", 4)
	if len(parts) != 4 {
		return fmt.Errorf("invalid credential key")
	}
	var credVer uint64
	if _, err := fmt.Sscanf(parts[1], "%d", &credVer); err != nil || credVer == 0 {
		return fmt.Errorf("invalid credential key version")
	}
	secret := parts[2]

	kConnect, _, _, err := protocol.DeriveKeys(secret, c.hostID)
	if err != nil {
		return fmt.Errorf("derive keys: %w", err)
	}

	c.logf("starting aic-host v%s [%s/%s] (host=%s)", c.options().Version, c.options().DeviceType, c.options().DeviceName, c.hostID)

	natsURL := ResolveNATSURL(c.options().Host)
	opts := []nats.Option{
		nats.Name("aic-host-" + c.hostID),
		nats.TokenHandler(func() string {
			return protocol.GenerateConnectToken(c.hostID, c.uid, c.options().Version, c.options().DeviceType, c.options().DeviceName,
				clockNowMS(), mustNonce(), kConnect)
		}),
		nats.ReconnectWait(2 * time.Second),
		nats.MaxReconnects(-1),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			c.logf("NATS reconnected, republishing caps")
			c.publishCaps(nc)
			go c.syncClock()
			writeState(State{Connected: true, HostID: c.hostID, NATSURL: natsURL, Version: c.options().Version})
		}),
		nats.DisconnectErrHandler(func(nc *nats.Conn, err error) {
			c.logf("NATS disconnected: %v", err)
			if isAuthError(err) {
				c.logf("FATAL: authentication permanently failed — credential expired or revoked. Obtain a new credential and restart.")
				writeState(State{Connected: false, HostID: c.hostID, NATSURL: natsURL, Version: c.options().Version,
					LastError: "authentication failed — credential expired or revoked (obtain a new credential and restart)"})
				go func() {
					c.lifecycleMu.Lock()
					defer c.lifecycleMu.Unlock()
					if c.connection() == nc {
						c.stopRTC()
						c.closeConnection()
					}
				}()
			} else if err != nil {
				writeState(State{Connected: false, HostID: c.hostID, NATSURL: natsURL, Version: c.options().Version,
					LastError: err.Error(), Retrying: true})
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

	// 链前对时：连接 token 与后续请求时效窗口都以平台时间为基准
	c.syncClock()

	nc, err := nats.Connect(natsURL, opts...)
	if err != nil {
		return fmt.Errorf("nats connect: %w", err)
	}
	c.logf("connected to NATS: %s", natsURL)

	c.publishCaps(nc)

	inbox, err := protocol.HostInboxSubject(c.uid, c.hostID)
	if err != nil {
		nc.Close()
		return err
	}
	// 每个请求独立 goroutine：避免 handler 阻塞造成 head-of-line 阻塞
	if _, err := nc.Subscribe(inbox, func(msg *nats.Msg) { go c.handleMsg(msg) }); err != nil {
		nc.Close()
		return fmt.Errorf("subscribe: %w", err)
	}
	c.logf("listening on %s", inbox)

	c.installConnection(nc)

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
	writeState(State{Connected: true, HostID: c.hostID, NATSURL: natsURL, Version: c.options().Version})
	return nil
}

// Close 优雅关闭：关闭 RTC 服务 → 取消订阅 → 断开 NATS。
func (c *Client) Close() error {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	c.closeConnection()
	if c.browserCancel != nil {
		c.browserCancel()
	}
	if c.mcpServices != nil {
		c.mcpServices.Close()
		if c.mcpServices.Started("browser") {
			c.closeBrowser(c.browserConfig)
		}
	}
	shutdown, cancelRuns := context.WithTimeout(context.Background(), 5*time.Second)
	_ = c.procs.Close(shutdown)
	cancelRuns()
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
	return nil
}

// Reconfigure 应用新运行配置（保存设置后调用）：
// 保留 Client 与 vbox Manager（bg 任务原样保留），仅更新
// work_dir/exec_timeout 参数；NATS 地址（host）变化时重连。
// 凭证/身份字段不变（换绑走 bind 流程重建）。
func (c *Client) Reconfigure(o cfg.Options) error {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	if c.closed {
		return fmt.Errorf("host client closed")
	}
	opts, err := optionsOf(o, c.options().DeviceType, c.options().Version, c.options().OnLog)
	if err != nil {
		return err
	}
	// 凭证与身份字段保持现有会话不变
	opts.Key = c.options().Key
	opts.DeviceName = c.options().DeviceName
	if opts.WorkDir == "" {
		opts.WorkDir = defaultWorkDir()
	}
	oldURL := ResolveNATSURL(c.options().Host)
	restartRTC := c.options().WorkDir != opts.WorkDir || c.options().RTC != opts.RTC || c.options().Transfers != opts.Transfers
	if restartRTC {
		c.stopRTC()
	}
	restartMCP := !reflect.DeepEqual(c.options().MCP, opts.MCP) || !reflect.DeepEqual(c.mcpAuth, cfg.AuthSnapshot()) || c.options().NoSandbox != opts.NoSandbox || c.options().WorkDir != opts.WorkDir
	c.procs.SetNoSandbox(opts.NoSandbox)
	// 授权模型同步（三域）：work_dir 变更 + 配置重载
	//（九键经 cfg.Global 由 api.SetConfig 先行更新）。
	c.policy.SetWorkDir(opts.WorkDir)
	if err := c.syncAuth(); err != nil {
		return err
	}
	c.optsMu.Lock()
	c.opts = opts
	c.optsMu.Unlock()
	if restartMCP {
		if err := c.configureMCP(opts.MCP); err != nil {
			return err
		}
		c.execGrantMu.Lock()
		for sid, names := range c.execGrants {
			keep := names[:0]
			for _, name := range names {
				if !strings.HasPrefix(name, "mcp.") {
					keep = append(keep, name)
				}
			}
			c.execGrants[sid] = keep
		}
		c.execGrantMu.Unlock()
	}
	if c.files != nil {
		_, home, osHome, err := deviceFileRoots(opts.WorkDir)
		if err != nil {
			return err
		}
		c.files.Configure(home, osHomePtr(osHome), opts.Transfers.ProxyUploadBytes)
		c.bytes.Configure(opts.Transfers.MaxUploadBytes, opts.Transfers.MaxSources)
	}
	nc := c.connection()
	if restartRTC && nc != nil {
		if err := c.startCommands(); err != nil {
			return err
		}
		if opts.RTC {
			if err := c.startRTC(); err != nil {
				return err
			}
		}
	}
	if nc != nil {
		c.publishCaps(nc)
	}
	if ResolveNATSURL(opts.Host) != oldURL {
		c.closeConnection()
		return c.connect()
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
		Browser:       c.streamBrowser,
		HostID:        c.hostID,
		Hostname:      hostname,
		Version:       c.options().Version,
		Send: func(sig *protocol.RtcSignal) {
			nc := c.connection()
			if nc == nil {
				return
			}
			subj, err := protocol.RtcOutSubject(c.uid, c.hostID, c.credVer)
			if err != nil {
				return
			}
			data, _ := json.Marshal(sig)
			nc.Publish(subj, data)
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
	var sig protocol.RtcSignal
	if err := json.Unmarshal(data, &sig); err != nil {
		return
	}
	svc.HandleSignal(&sig)
}

// ---- caps v2 上报（§6.3） ----

// buildCaps 声明原生执行与文件传输能力；mcp 只是 exec 内的命令。
func (c *Client) buildCaps() *protocol.Caps {
	hostname, _ := os.Hostname()
	actions := append([]string(nil), fsx.FSActions...)
	return &protocol.Caps{
		HostID:        c.hostID,
		CredentialVer: c.credVer,
		AgentVersion:  c.options().Version,
		DeviceType:    c.options().DeviceType,
		Hostname:      hostname,
		DeviceInfo:    deviceInfo(),
		Mgmt:          c.buildMgmt(),
		ToolProtocols: []string{protocol.NatsProtocol, protocol.RtcProtocol},
		FS:            protocol.FSCaps{Actions: &actions},
	}
}

// buildMgmt advertises only the live generic transport, never a management code.
func (c *Client) buildMgmt() *protocol.MgmtCaps {
	c.rtcMu.RLock()
	defer c.rtcMu.RUnlock()
	if c.files == nil {
		return nil
	}
	m := &protocol.MgmtCaps{Transports: map[string]protocol.TransportCaps{"proxy": {Enabled: true, Protocol: protocol.NatsProtocol, Commands: []string{"fs"}}}}
	if c.rtcSvc != nil {
		commands := []string{"fs", "exec"}

		m.Transports["rtc"] = protocol.TransportCaps{Enabled: true, Protocol: protocol.RtcProtocol, Commands: commands}
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
	subj, err := protocol.CapsSubject(c.uid, c.hostID, c.credVer)
	if err != nil {
		c.logf("caps subject: %v", err)
		return
	}
	data, _ := json.Marshal(c.buildCaps())
	nc.Publish(subj, data)
	c.logf("caps published to %s", subj)
}

func (c *Client) connection() *nats.Conn {
	c.ncMu.RLock()
	defer c.ncMu.RUnlock()
	return c.nc
}

// Lifecycle callers are serialized. Each heartbeat owns one immutable NATS
// connection and must exit before a replacement is installed.
func (c *Client) installConnection(nc *nats.Conn) {
	c.closeConnection()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	c.ncMu.Lock()
	c.nc, c.heartbeatCancel, c.heartbeatDone = nc, cancel, done
	c.ncMu.Unlock()
	go func() { defer close(done); c.heartbeatLoop(ctx, nc) }()
}

func (c *Client) closeConnection() {
	c.ncMu.Lock()
	nc, cancel, done := c.nc, c.heartbeatCancel, c.heartbeatDone
	c.nc, c.heartbeatCancel, c.heartbeatDone = nil, nil, nil
	c.ncMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if nc != nil {
		nc.Close()
	}
	if done != nil {
		<-done
	}
}

func (c *Client) heartbeatLoop(ctx context.Context, nc *nats.Conn) {
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	syncTicker := time.NewTicker(clockSyncInterval)
	defer syncTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-syncTicker.C:
			c.syncClock()
			continue
		case <-ticker.C:
		}
		if nc.IsClosed() {
			return
		}
		subj, err := protocol.PresenceSubject(c.uid, c.hostID, c.credVer)
		if err != nil {
			continue
		}
		presence := map[string]any{
			"host_id":        c.hostID,
			"credential_ver": c.credVer,
			"running":        1,
			"sent_at":        clockNow().UTC().Format(time.RFC3339),
		}
		data, _ := json.Marshal(presence)
		nc.Publish(subj, data)
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

func (c *Client) options() Options { c.optsMu.RLock(); defer c.optsMu.RUnlock(); return c.opts }
