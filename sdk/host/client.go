// Package host 是 AIC host agent 运行时（docs/instruction_sets_v2.md §6.2）：
// NATS 连接与认证、caps v2 上报、心跳、req 分发（fs/exec）、bg 注册表、
// granted_level 纵深检查（与 vcore 分级表同源）。
//
// 物理 host 命令空间 = 统一命令声明表（§5.1）：恒声明（核心 8 虚拟指令 +
// commands + bg_*）+ 启动探测（shell/git/agent-browser CLI，exec.LookPath）。
// 未声明的命令一律拒绝，不存在「未知命令透传」。
package host

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/veypi/aic-pod/sdk/exec_procs"
	"github.com/veypi/aic-pod/sdk/proto"
	"github.com/veypi/aic-pod/sdk/vcore"
	vbrowser "github.com/veypi/aic-pod/sdk/vcore/browser"
)

// Options 客户端配置。
type Options struct {
	Host        string        // 平台地址（如 https://ivec.ai，可带路径前缀），NATS 端点据此推断
	Key         string        // "<host_id>.<cred_ver>.<secret>.<uid>"（必填）
	WorkDir     string        // exec/fs 缺省工作区（§2.1.1 workdir 缺省值），默认 /tmp
	DeviceName  string        // 展示名称，默认 hostname
	DeviceType  string        // 客户端类型（cli/browser/...），默认 cli
	Version     string        // 客户端版本号（va.b.c，§6.3 版本门禁）
	ExecTimeout time.Duration // 程序后台自有超时，默认 30m（§5.9）
	OnLog       func(format string, args ...any)
}

// Client 是 host agent 客户端。
type Client struct {
	opts      Options
	nc        *nats.Conn
	kTool     string
	hostID    string
	uid       string
	credVer   uint64
	replay    *replayCache
	cmds      []proto.CommandDecl          // 统一命令声明表（§5.1：恒声明 + 启动探测）
	cmdByName map[string]proto.CommandDecl // cmds 的 name 索引（路由与纵深检查用）
	procs     *exec_procs.Manager          // exec 子进程统一托管（§5.8/§5.9）
	browserMu sync.Mutex
	browsers  map[string]*vbrowser.Browser // per-session browser 实例（pod 模式不隔离，§5.6）
	logf      func(string, ...any)
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
	c := &Client{
		opts:     opts,
		replay:   &replayCache{store: map[string]time.Time{}},
		procs:    exec_procs.NewManager(opts.ExecTimeout),
		browsers: map[string]*vbrowser.Browser{},
		logf:     logf,
	}
	c.cmds, c.cmdByName = buildCommandTable()
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
	return nil
}

// Close 优雅关闭：取消订阅 → 断开 NATS。
func (c *Client) Close() error {
	if c.nc != nil {
		c.nc.Close()
		c.nc = nil
	}
	return nil
}

// ---- caps v2 上报（§6.3） ----

// buildCommandTable 构建物理 host 的统一命令声明表（§5.1）：
//   - 恒声明：核心 8 虚拟指令 + commands + bg_list/bg_wait/bg_kill（vcore 元数据同源）
//   - 启动探测（exec.LookPath，探测到才声明）：
//     shell（bash/zsh/sh/fish；Windows: powershell/pwsh/cmd）→ level 3（逃生舱）；
//     git → level 1（本地凭证天然可用）；browser → agent-browser CLI
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
	for _, name := range []string{"commands", "bg_list", "bg_wait", "bg_kill"} {
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
	if _, err := exec.LookPath("agent-browser"); err == nil {
		if d, ok := vcore.Decl("browser"); ok {
			add(d)
		}
	}
	byName := make(map[string]proto.CommandDecl, len(cmds))
	for _, d := range cmds {
		byName[d.Name] = d
	}
	return cmds, byName
}

// buildCaps 构造物理 host 的 caps v2（§6.3）：
// fs.actions=null（全部 3 个）；exec.commands = 统一命令声明表。
func (c *Client) buildCaps() *proto.Caps {
	hostname, _ := os.Hostname()
	return &proto.Caps{
		HostID:        c.hostID,
		CredentialVer: c.credVer,
		AgentVersion:  c.opts.Version,
		DeviceType:    c.opts.DeviceType,
		Hostname:      hostname,
		DeviceInfo:    deviceInfo(),
		FS:            proto.FSCaps{},                   // actions=null = 全部 3 个
		Exec:          proto.ExecCaps{Commands: c.cmds}, // 统一命令声明表
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
	c.logf("caps published to %s (%d commands)", subj, len(c.cmds))
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
