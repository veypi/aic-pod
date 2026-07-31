package aichost

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"
)

// unsignedToolGrantedLevel 是 Unsigned 工具的固定执行等级：owner 浏览器直连
// 语义，等价于用户本人操作（授权已由 natsauth subject 所有权保证）。
const unsignedToolGrantedLevel = 3

// Client 是 AIC Env 客户端。
type Client struct {
	opts    Options
	nc      *nats.Conn
	kTool   string
	hostID  string
	hostUID string
	credVer uint64
	tools   []Tool
	cache   idempotentCache
	replay  replayCache
	logf    func(string, ...any)
}

// New 创建客户端，不连接。可在此之后 RegisterTool 注册自定义工具。
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
	if opts.ExecTimeout <= 0 {
		opts.ExecTimeout = 10 * time.Minute
	}
	logf := opts.OnLog
	if logf == nil {
		logf = func(format string, args ...any) {
			ts := time.Now().Format("15:04:05")
			fmt.Printf("[%s] %s\n", ts, fmt.Sprintf(format, args...))
		}
	}
	return &Client{
		opts: opts,
		logf: logf,
		tools: []Tool{
			ExecTool(opts.WorkDir, opts.ExecTimeout),
			FsTool(),
			HfsTool(),
		},
	}
}

// RegisterTool 注册自定义工具。必须在 Connect 前调用。
func (c *Client) RegisterTool(t Tool) {
	c.tools = append(c.tools, t)
}

// Connect 连接 NATS，发布 CAPS，订阅工具请求，启动心跳。后台运行，即时返回。
func (c *Client) Connect() error {
	parts := strings.SplitN(c.opts.Credential, ".", 4)
	if len(parts) != 4 {
		return fmt.Errorf("invalid credential format: expected <host_id>.<cred_ver>.<secret>.<uid>")
	}
	c.hostID = parts[0]
	if _, err := fmt.Sscanf(parts[1], "%d", &c.credVer); err != nil || c.credVer == 0 {
		return fmt.Errorf("invalid cred_ver in credential: %s", parts[1])
	}
	secret := parts[2]
	c.hostUID = parts[3]

	kConnect, _, kTool, err := deriveKeys(secret, c.hostID)
	if err != nil {
		return fmt.Errorf("derive keys: %w", err)
	}
	c.kTool = kTool

	c.logf("starting aic-host v%s [%s/%s] (env=%s)", c.opts.Version, c.opts.DeviceType, c.opts.DeviceName, c.hostID)

	natsURL := c.opts.NATSURL
	opts := []nats.Option{
		nats.Name("aic-host-" + c.hostID),
		nats.TokenHandler(func() string {
			return generateConnectToken(c.hostID, c.hostUID, c.opts.Version, c.opts.DeviceType, c.opts.DeviceName, kConnect)
		}),
		nats.ReconnectWait(2 * time.Second),
		nats.MaxReconnects(-1),
		nats.CustomReconnectDelay(func(attempts int) time.Duration {
			// 指数退避：2s → 4s → 8s → 16s → 30s (上限)
			delay := time.Duration(1<<min(attempts, 5)) * time.Second
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}
			return delay
		}),
		nats.DisconnectErrHandler(func(nc *nats.Conn, err error) {
			c.logf("NATS disconnected: %v", err)
			if isAuthError(err) {
				c.logf("FATAL: authentication permanently failed — credential expired or revoked. Obtain a new ENV_KEY and restart.")
				go nc.Close()
			}
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			c.logf("NATS reconnected, republishing caps")
			c.publishCaps(nc)
		}),
		nats.ErrorHandler(func(nc *nats.Conn, _ *nats.Subscription, err error) {
			c.logf("NATS error: %v", err)
			if isAuthError(err) {
				c.logf("FATAL: authentication permanently failed — credential expired or revoked. Obtain a new ENV_KEY and restart.")
				go nc.Close()
			}
		}),
	}

	if strings.HasPrefix(natsURL, "ws") {
		if u, err := url.Parse(natsURL); err == nil && u.Path != "" && u.Path != "/" {
			opts = append(opts, nats.ProxyPath(u.Path))
			natsURL = fmt.Sprintf("%s://%s", u.Scheme, u.Host)
			c.logf("ws proxy path: %s → %s", u.Path, natsURL)
		}
	}

	nc, err := nats.Connect(natsURL, opts...)
	if err != nil {
		return fmt.Errorf("nats connect: %w", err)
	}
	c.nc = nc

	c.logf("connected to NATS: %s", natsURL)

	c.publishCaps(nc)

	toolWildcard := fmt.Sprintf("u.%s.h.%s.%d.tool.*.req", c.hostUID, c.hostID, c.credVer)
	// 每个请求独立 goroutine 处理：nats 异步订阅由单个分发 goroutine 串行投递，
	// 任一 handler 阻塞（如读取 /proc 伪文件永不返回）会造成 head-of-line 阻塞，
	// 后续所有工具请求排队至 server 端超时。
	if _, err := nc.Subscribe(toolWildcard, func(msg *nats.Msg) { go c.handleToolRequest(msg) }); err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	c.logf("listening on %s", toolWildcard)

	go c.heartbeatLoop()

	return nil
}

// Wait 阻塞等待 SIGINT/SIGTERM，收到信号后自动 Close。
func (c *Client) Wait() {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	c.logf("shutting down...")
	c.Close()
}

// Close 优雅关闭：取消订阅 → 断开 NATS。
func (c *Client) Close() error {
	if c.nc != nil {
		c.nc.Close()
		c.nc = nil
	}
	return nil
}

// ---- 内部方法 ----

func (c *Client) publishCaps(nc *nats.Conn) {
	hostname, _ := os.Hostname()
	toolDefs := make([]ToolDef, len(c.tools))
	for i, t := range c.tools {
		toolDefs[i] = t.Def
	}
	caps := capabilities{
		HostID:        c.hostID,
		AgentVersion:  c.opts.Version,
		CredentialVer: c.credVer,
		DeviceType:    c.opts.DeviceType,
		DeviceName:    c.opts.DeviceName,
		DeviceInfo: &hostDeviceInfo{
			Hostname:  hostname,
			OS:        runtime.GOOS,
			Arch:      runtime.GOARCH,
			NumCPU:    runtime.NumCPU(),
			GoVersion: runtime.Version(),
		},
		Tools: toolDefs,
	}
	data, _ := json.Marshal(caps)
	subj := fmt.Sprintf("u.%s.h.%s.%d.caps", c.hostUID, c.hostID, c.credVer)
	nc.Publish(subj, data)
	c.logf("caps published to %s (%d tools)", subj, len(caps.Tools))
}

func (c *Client) heartbeatLoop() {
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if c.nc == nil {
			return
		}
		presence := map[string]any{
			"host_id":        c.hostID,
			"credential_ver": c.credVer,
			"running":        1,
			"sent_at":        time.Now().UTC().Format(time.RFC3339),
		}
		data, _ := json.Marshal(presence)
		c.nc.Publish(fmt.Sprintf("u.%s.h.%s.%d.presence", c.hostUID, c.hostID, c.credVer), data)
	}
}

func (c *Client) handleToolRequest(msg *nats.Msg) {
	var req toolRequest
	if err := json.Unmarshal(msg.Data, &req); err != nil {
		c.respond(msg, toolResponse{State: "error", Error: "invalid request: " + err.Error()})
		return
	}

	c.logf("→ request: %s deadline=%s (msg=%s)", req.ToolName, req.Deadline, req.MsgID)

	t := c.findTool(req.ToolName)
	if t == nil {
		c.logf("tool request: unknown tool %s", req.ToolName)
		c.respond(msg, toolResponse{State: "error", Error: fmt.Sprintf("unknown tool: %s", req.ToolName)})
		return
	}

	// ---- 防重放（§10.1 host 端验证规范，必须实现） ----

	// 1. 验签（Unsigned 工具除外：授权依赖 natsauth subject 所有权——仅 owner
	//    前端连接与 aic server 可发布到本 subject；此类工具固定以 owner 直连
	//    等级执行，信封自报的 GrantedLevel 不可信、直接覆盖）
	if t.Unsigned {
		req.GrantedLevel = unsignedToolGrantedLevel
	} else {
		if req.Signature == "" {
			c.logf("tool request rejected: missing K_tool signature for %s", req.ToolName)
			c.respond(msg, toolResponse{State: "rejected", Error: "missing request signature"})
			return
		}
		if !verifyToolRequestSig(&req, c.hostID, c.kTool) {
			c.logf("tool request rejected: invalid K_tool signature for %s", req.ToolName)
			c.respond(msg, toolResponse{State: "rejected", Error: "invalid request signature"})
			return
		}
	}

	// 2. deadline 过期拒绝
	var deadline time.Time
	if req.Deadline != "" {
		dl, err := time.Parse(time.RFC3339, req.Deadline)
		if err == nil {
			deadline = dl
			if time.Now().After(dl) {
				c.respond(msg, toolResponse{State: "rejected", Error: "request expired"})
				return
			}
		}
	}

	// 3. nonce 窗口内缓存去重：同一 nonce 的重复请求（网络重传/恶意重放）直接拒绝
	if req.Nonce != "" && !c.replay.checkAndMark(req.Nonce, deadline) {
		c.logf("tool request rejected: duplicate nonce (msg=%s)", req.MsgID)
		c.respond(msg, toolResponse{State: "rejected", Error: "duplicate nonce"})
		return
	}

	// action 级权限检查（§10.2：caps 声明的 action 级等级优先，未声明继承指令集基线）。
	// 审批模型见 aic docs/tool_permission.md：审批通过 = procs 以 granted_level=9 重发，
	// host 只做 granted >= required 数字比较；不足返回 waiting 转人工审批（非 rejected）。
	params, _ := ParseToolParams(req.ToolData)
	requiredLevel := actionRequiredLevel(t.Def, params.Action)
	if req.GrantedLevel < requiredLevel {
		reason := fmt.Sprintf("%s %s requires level %d (granted %d)", req.ToolName, params.Action, requiredLevel, req.GrantedLevel)
		c.logf("tool request waiting approval: %s (msg=%s)", reason, req.MsgID)
		c.respond(msg, toolResponse{
			MsgID:        req.MsgID,
			State:        "waiting",
			NeedApproval: &NeedApprovalDetail{Reason: reason},
		})
		return
	}

	if cached := c.cache.get(req.MsgID); cached != nil {
		c.logf("returning cached result for %s", req.MsgID)
		cached.MsgID = req.MsgID
		c.respond(msg, *cached)
		return
	}

	c.logf("tool request: %s %s (msg=%s)", req.ToolName, formatCmd(params), req.MsgID)

	ctx := context.WithValue(context.Background(), reqCtxKey{}, &RequestCtx{
		GrantedLevel: req.GrantedLevel,
		SessionID:    req.SessionID,
		MsgID:        req.MsgID,
	})
	if !deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}

	result, err := t.Handler(ctx, req.ToolData)
	if err != nil {
		c.respond(msg, toolResponse{State: "error", Error: err.Error()})
		return
	}

	resp := toolResponse{
		MsgID:        req.MsgID,
		Content:      result.Content,
		Error:        result.Error,
		Attrs:        result.Attrs,
		NeedApproval: result.NeedApproval,
	}

	switch result.State {
	case "rejected":
		resp.State = "rejected"
	case "waiting":
		resp.State = "waiting"
	default:
		if result.Error != "" {
			resp.State = "error"
		} else {
			resp.State = "completed"
		}
	}

	// 仅对成功结果做幂等缓存，waiting/rejected/error 不缓存
	if resp.State == "completed" {
		c.cache.set(req.MsgID, &resp)
	}
	c.respond(msg, resp)
}

// actionRequiredLevel 解析 action 级权限：caps actions 对象形式声明优先，
// 字符串简写与未声明 action 继承指令集级 RequiredLevel（§10.2）。
func actionRequiredLevel(def ToolDef, action string) int {
	for _, a := range def.Actions {
		if m, ok := a.(map[string]any); ok {
			if name, _ := m["name"].(string); name == action {
				if lv, ok := m["required_level"].(float64); ok {
					return int(lv)
				}
				if lv, ok := m["required_level"].(int); ok {
					return lv
				}
			}
		}
	}
	return def.RequiredLevel
}

func (c *Client) findTool(name string) *Tool {
	for i := range c.tools {
		if c.tools[i].Def.Name == name {
			return &c.tools[i]
		}
	}
	return nil
}

func (c *Client) respond(msg *nats.Msg, resp toolResponse) {
	data, _ := json.Marshal(resp)
	msg.Respond(data)
	c.logf("← response: msg=%s state=%s error=%q", resp.MsgID, resp.State, resp.Error)
}

func isAuthError(err error) bool {
	s := err.Error()
	return strings.Contains(s, "authentication") || strings.Contains(s, "authorization")
}

func formatCmd(params *ToolParams) string {
	if params == nil || params.Action == "" {
		return "(empty)"
	}
	s := params.Action
	if len(params.Argv) > 0 {
		s += " " + strings.Join(params.Argv, " ")
	}
	if len(s) > 80 {
		s = s[:80] + "..."
	}
	return s
}
