package host

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/veypi/aic-pod/sdk/proto"
	"github.com/veypi/aic-pod/sdk/vcore"
)

// handleMsg 处理一条工具请求（§6.2 host 端验证规范，必须实现）：
// 验签 → deadline 过期拒绝 → nonce 窗口去重 → granted_level 纵深检查 → 分发。
func (c *Client) handleMsg(msg *nats.Msg) {
	resp := c.dispatch(context.Background(), msg.Subject, msg.Data)
	if resp == nil {
		return
	}
	data, _ := json.Marshal(resp)
	msg.Respond(data)
	c.logf("← response: msg=%s state=%s error=%q", resp.MsgID, resp.State, resp.Error)
}

// dispatch 是请求处理主流程（与 NATS 解耦，可单测）。
func (c *Client) dispatch(ctx context.Context, subject string, data []byte) *proto.ToolResponse {
	var req proto.ToolRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return &proto.ToolResponse{State: proto.StateError, Error: "invalid request: " + err.Error()}
	}
	c.logf("→ request: %s %s (msg=%s)", req.Tool, subject, req.MsgID)

	// 1. 验签
	if req.Sig == "" || !proto.VerifyToolRequest(&req, c.hostID, c.kTool) {
		return reject(req.MsgID, "invalid request signature")
	}

	// 2. deadline 非法/过期拒绝（非空但不可解析 → 拒绝，防"永不过期"请求）
	var deadline time.Time
	if req.Deadline != "" {
		dl, err := time.Parse(time.RFC3339, req.Deadline)
		if err != nil {
			return reject(req.MsgID, "invalid deadline")
		}
		deadline = dl
		if time.Now().After(dl) {
			return reject(req.MsgID, "request expired")
		}
	}

	// 3. nonce 必填且窗口内缓存去重（空 nonce 直接拒绝，防跳过去重）
	if req.Nonce == "" {
		return reject(req.MsgID, "missing nonce")
	}
	if !c.replay.checkAndMark(req.Nonce, deadline) {
		return reject(req.MsgID, "duplicate nonce")
	}

	// 4. granted_level 纵深检查（§2.4 判定分工：host 端按 caps 声明 + 本地规则再自检）
	if state, reason := c.checkGranted(&req); state != "" {
		return &proto.ToolResponse{
			MsgID: req.MsgID, State: state,
			NeedApproval: &proto.NeedApproval{Reason: reason},
		}
	}

	if !deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}

	// 5. 分发
	_, sid, _, _, _ := proto.ParseToolReqSubject(subject)
	switch req.Tool {
	case proto.ToolFS:
		return c.execFS(ctx, &req)
	case proto.ToolExec:
		return c.execCmd(ctx, sid, &req)
	}
	return &proto.ToolResponse{MsgID: req.MsgID, State: proto.StateError,
		Error: fmt.Sprintf("unknown tool %q (supported: fs, exec)", req.Tool)}
}

// checkGranted 做 granted >= required 数字比较（与 vcore 分级表同源）。
// 不足返回 waiting + reason（§6.2：host 端动态审批，服务端置 waiting 等用户审批）。
func (c *Client) checkGranted(req *proto.ToolRequest) (proto.State, string) {
	required := proto.LevelDanger
	switch req.Tool {
	case proto.ToolFS:
		var p struct {
			Action string `json:"action"`
		}
		_ = json.Unmarshal(req.Data, &p)
		required = vcore.FSRequired(p.Action)
	case proto.ToolExec:
		var p struct {
			Action string   `json:"action"`
			Argv   []string `json:"argv"`
		}
		_ = json.Unmarshal(req.Data, &p)
		if p.Action == "commands" {
			required = proto.LevelRead
		} else if isVirtual(p.Action) {
			required = vcore.ExecRequiredIn(c.newEnv(""), p.Action, p.Argv)
		}
		// 程序命令按基线 Danger(3)（§2.4）
	}
	if req.GrantedLevel < required {
		return proto.StateWaiting, fmt.Sprintf("%s %s requires level %d (granted %d)",
			req.Tool, actionOf(req), required, req.GrantedLevel)
	}
	return "", ""
}

func actionOf(req *proto.ToolRequest) string {
	var p struct {
		Action string `json:"action"`
	}
	_ = json.Unmarshal(req.Data, &p)
	return p.Action
}

// execFS 执行 fs 请求（vcore + OS VFS 适配）。
func (c *Client) execFS(ctx context.Context, req *proto.ToolRequest) *proto.ToolResponse {
	res, err := vcore.RunFS(ctx, c.newEnv(""), req.Data)
	return resultToResponse(req.MsgID, res, err)
}

// newEnv 构建 OS 文件系统执行环境。
// workdir 为空时使用 host 端配置工作区（§2.1.1 缺省值）。
func (c *Client) newEnv(workdir string) *vcore.Env {
	if workdir == "" {
		workdir = c.opts.WorkDir
	}
	return &vcore.Env{
		VFS:          OSVFS{},
		Workdir:      workdir,
		ProtectRoots: filesystemRoots(),
		Fetcher:      httpFetcher{}, // 物理 host 不限制 SSRF（用户本机网络属其自身边界，§5.4）
		ImageData:    true,          // host 端图片经 image_data 返回（§2.2）
	}
}

// isVirtual 判定 action 是否为虚拟指令（同名虚拟优先，§5.4：
// 真实核心指令程序不直接暴露，完整 GNU/BSD 语义走 shell 逃生舱）。
func isVirtual(action string) bool {
	for _, n := range vcore.CoreCommandNames() {
		if n == action {
			return true
		}
	}
	switch action {
	case "commands", "bg_list", "bg_wait", "bg_kill":
		return true
	}
	return false
}

// execCmd 执行 exec 请求：虚拟指令优先，否则按程序处理（§5.9）。
func (c *Client) execCmd(ctx context.Context, sid string, req *proto.ToolRequest) *proto.ToolResponse {
	var p struct {
		Action  string   `json:"action"`
		Argv    []string `json:"argv"`
		Workdir string   `json:"workdir"`
	}
	if err := json.Unmarshal(req.Data, &p); err != nil {
		return &proto.ToolResponse{MsgID: req.MsgID, State: proto.StateError, Error: "invalid exec data: " + err.Error()}
	}
	if p.Action == "" {
		return &proto.ToolResponse{MsgID: req.MsgID, State: proto.StateError,
			Error: "exec: action is required"}
	}

	env := c.newEnv(p.Workdir)
	switch p.Action {
	case "commands":
		return &proto.ToolResponse{MsgID: req.MsgID, State: proto.StateCompleted,
			Content: c.commandsJSON(), Attrs: map[string]string{"action": "commands"}}
	case "bg_list":
		return resultToResponse(req.MsgID, c.bgs.list(sid), nil)
	case "bg_wait":
		res, err := c.bgs.wait(ctx, sid, p.Argv)
		return resultToResponse(req.MsgID, res, err)
	case "bg_kill":
		res, err := c.bgs.kill(sid, p.Argv)
		return resultToResponse(req.MsgID, res, err)
	}

	if isVirtual(p.Action) {
		res, err := vcore.Run(ctx, env, p.Action, p.Argv)
		return resultToResponse(req.MsgID, res, err)
	}

	// 程序命令（§5.9）：PATH 查找、workdir = 进程 cwd、日志文件、deadline 超时自动后台化
	return c.runProgram(ctx, sid, req.MsgID, p.Action, p.Argv, p.Workdir)
}

// commandsJSON 返回本 host 的 caps v2 能力（§5.2：programs=null 不限制）。
func (c *Client) commandsJSON() string {
	caps := c.buildCaps()
	data, _ := json.Marshal(map[string]any{
		"fs":       map[string]any{"actions": proto.AllFSActions},
		"programs": nil,
		"virtual":  caps.Exec.Virtual,
	})
	return string(data)
}

// resultToResponse 将 vcore.Result/错误映射为响应信封（§6.2 错误模型）。
func resultToResponse(msgID string, res *vcore.Result, err error) *proto.ToolResponse {
	if err != nil {
		state := proto.StateOf(err)
		resp := &proto.ToolResponse{MsgID: msgID, State: state, Error: err.Error()}
		if ae, ok := err.(*proto.ApprovalError); ok {
			resp.NeedApproval = &proto.NeedApproval{Reason: ae.Reason, Preview: ae.Preview}
		}
		return resp
	}
	return &proto.ToolResponse{MsgID: msgID, State: proto.StateCompleted,
		Content: res.Content, Attrs: res.Attrs}
}

func reject(msgID, reason string) *proto.ToolResponse {
	return &proto.ToolResponse{MsgID: msgID, State: proto.StateRejected, Error: reason}
}

func deviceInfo() *proto.DeviceInfo {
	return &proto.DeviceInfo{OS: runtime.GOOS, Arch: runtime.GOARCH, NumCPU: runtime.NumCPU()}
}

func mustNonce() string {
	n, err := proto.NewNonce()
	if err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return n
}

type wsURL struct {
	base      string
	proxyPath string
}

func parseWSURL(raw string) (*wsURL, error) {
	// ws(s)://host/path → base = ws(s)://host，path 作为 ProxyPath。
	// 扫描起点须按实际 scheme（ws:// 与 wss:// 长度不同）跳过 "://"，
	// 否则 wss:// 的第二个斜杠会被误判为路径起点。
	i := strings.Index(raw, "://")
	if i < 0 {
		return &wsURL{base: raw}, nil
	}
	for j := i + 3; j < len(raw); j++ {
		if raw[j] == '/' {
			if j == len(raw)-1 {
				return &wsURL{base: raw}, nil
			}
			return &wsURL{base: raw[:j], proxyPath: raw[j:]}, nil
		}
	}
	return &wsURL{base: raw}, nil
}

var _ = nats.ErrNoResponders
