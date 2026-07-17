package aicenv

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

// Client 是 AIC Env 客户端。
type Client struct {
	opts    Options
	nc      *nats.Conn
	kTool   string
	envID   string
	envUID  string
	credVer uint64
	tools   []Tool
	cache   idempotentCache
	logf    func(string, ...any)
}

// New 创建客户端，不连接。可在此之后 RegisterTool 注册自定义工具。
func New(opts Options) *Client {
	if opts.DeviceType == "" {
		opts.DeviceType = "device"
	}
	if opts.DeviceName == "" {
		opts.DeviceName, _ = os.Hostname()
	}
	if opts.WorkDir == "" {
		opts.WorkDir = os.TempDir()
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
			ExecTool(opts.WorkDir),
			FsTool(),
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
		return fmt.Errorf("invalid credential format: expected <env_id>.<cred_ver>.<secret>.<uid>")
	}
	c.envID = parts[0]
	if _, err := fmt.Sscanf(parts[1], "%d", &c.credVer); err != nil || c.credVer == 0 {
		return fmt.Errorf("invalid cred_ver in credential: %s", parts[1])
	}
	secret := parts[2]
	c.envUID = parts[3]

	kConnect, _, kTool, err := deriveKeys(secret, c.envID)
	if err != nil {
		return fmt.Errorf("derive keys: %w", err)
	}
	c.kTool = kTool

	c.logf("starting aic-env v%s [%s/%s] (env=%s)", c.opts.Version, c.opts.DeviceType, c.opts.DeviceName, c.envID)

	natsURL := c.opts.NATSURL
	opts := []nats.Option{
		nats.Name("aic-env-" + c.envID),
		nats.TokenHandler(func() string {
			return generateConnectToken(c.envID, c.envUID, c.opts.Version, c.opts.DeviceType, c.opts.DeviceName, kConnect)
		}),
		nats.ReconnectWait(2 * time.Second),
		nats.MaxReconnects(-1),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			c.logf("NATS disconnected: %v", err)
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			c.logf("NATS reconnected, republishing caps")
			c.publishCaps(nc)
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

	toolWildcard := fmt.Sprintf("u.%s.e.%s.%d.tool.*.req", c.envUID, c.envID, c.credVer)
	if _, err := nc.Subscribe(toolWildcard, c.handleToolRequest); err != nil {
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
		EnvID:         c.envID,
		AgentVersion:  c.opts.Version,
		CredentialVer: c.credVer,
		DeviceType:    c.opts.DeviceType,
		DeviceName:    c.opts.DeviceName,
		DeviceInfo: &envDeviceInfo{
			Hostname:  hostname,
			OS:        runtime.GOOS,
			Arch:      runtime.GOARCH,
			NumCPU:    runtime.NumCPU(),
			GoVersion: runtime.Version(),
		},
		Tools: toolDefs,
	}
	data, _ := json.Marshal(caps)
	subj := fmt.Sprintf("u.%s.e.%s.%d.caps", c.envUID, c.envID, c.credVer)
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
			"env_id":         c.envID,
			"credential_ver": c.credVer,
			"running":        1,
			"sent_at":        time.Now().UTC().Format(time.RFC3339),
		}
		data, _ := json.Marshal(presence)
		c.nc.Publish(fmt.Sprintf("u.%s.e.%s.%d.presence", c.envUID, c.envID, c.credVer), data)
	}
}

func (c *Client) handleToolRequest(msg *nats.Msg) {
	var req toolRequest
	if err := json.Unmarshal(msg.Data, &req); err != nil {
		c.respond(msg, toolResponse{Status: "error", Error: "invalid request: " + err.Error()})
		return
	}

	if req.Signature == "" {
		c.logf("tool request rejected: missing K_tool signature for %s", req.ToolName)
		c.respond(msg, toolResponse{Status: "rejected", Error: "missing request signature"})
		return
	}
	if !verifyToolRequestSig(&req, c.envID, c.kTool) {
		c.logf("tool request rejected: invalid K_tool signature for %s", req.ToolName)
		c.respond(msg, toolResponse{Status: "rejected", Error: "invalid request signature"})
		return
	}

	if req.Deadline != "" {
		dl, err := time.Parse(time.RFC3339, req.Deadline)
		if err == nil && time.Now().After(dl) {
			c.respond(msg, toolResponse{Status: "rejected", Error: "request deadline exceeded"})
			return
		}
	}

	t := c.findTool(req.ToolName)
	if t == nil {
		c.logf("tool request: unknown tool %s", req.ToolName)
		c.respond(msg, toolResponse{Status: "error", Error: fmt.Sprintf("unknown tool: %s", req.ToolName)})
		return
	}

	if req.GrantedLevel < t.Def.RequiredLevel && req.Approval == nil {
		c.logf("tool request denied: %s (granted=%d < required=%d)", req.ToolName, req.GrantedLevel, t.Def.RequiredLevel)
		c.respond(msg, toolResponse{Status: "rejected", Error: fmt.Sprintf("insufficient permission: %s requires level %d, got %d", req.ToolName, t.Def.RequiredLevel, req.GrantedLevel)})
		return
	}

	if cached := c.cache.get(req.MsgID); cached != nil {
		c.logf("returning cached result for %s", req.MsgID)
		cached.MsgID = req.MsgID
		c.respond(msg, *cached)
		return
	}

	c.logf("tool request: %s (msg=%s)", req.ToolName, req.MsgID)

	ctx := context.WithValue(context.Background(), reqCtxKey{}, &RequestCtx{
		GrantedLevel: req.GrantedLevel,
		Approved:     req.Approval != nil,
		ResolvedBy:   approvalResolvedBy(req),
		SessionID:    req.SessionID,
		MsgID:        req.MsgID,
	})
	if req.Deadline != "" {
		if dl, err := time.Parse(time.RFC3339, req.Deadline); err == nil {
			var cancel context.CancelFunc
			ctx, cancel = context.WithDeadline(ctx, dl)
			defer cancel()
		}
	}

	result, err := t.Handler(ctx, req.ToolData)
	if err != nil {
		c.respond(msg, toolResponse{Status: "error", Error: err.Error()})
		return
	}

	resp := toolResponse{
		MsgID:        req.MsgID,
		Content:      result.Content,
		Error:        result.Error,
		Attrs:        result.Attrs,
		NeedApproval: result.NeedApproval,
	}

	switch result.Status {
	case "rejected":
		resp.Status = "rejected"
	case "waiting":
		resp.Status = "waiting"
	default:
		if result.Error != "" {
			resp.Status = "error"
		} else {
			resp.Status = "completed"
		}
	}

	// 仅对成功结果做幂等缓存，waiting/rejected/error 不缓存
	if resp.Status == "completed" {
		c.cache.set(req.MsgID, &resp)
	}
	c.respond(msg, resp)
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
}

func approvalResolvedBy(req toolRequest) string {
	if req.Approval != nil {
		return req.Approval.ResolvedBy
	}
	return ""
}
