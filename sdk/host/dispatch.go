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

	// 2. deadline 必填且未过期（空 = "永不过期"请求，拒绝；格式非法拒绝）
	//    请求方为可信服务端（request.go 恒签发 RFC3339 deadline），空/非法均为异常。
	if req.Deadline == "" {
		return reject(req.MsgID, "missing deadline")
	}
	dl, err := time.Parse(time.RFC3339, req.Deadline)
	if err != nil {
		return reject(req.MsgID, "invalid deadline")
	}
	deadline := dl
	if time.Now().After(dl) {
		return reject(req.MsgID, "request expired")
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

	// 5. 分发（subject 带 sid 段定向——§6.1 v4；sid 仍从信封 SessionID 读取，
	//    会话隔离由信封提供（bg 命名空间 {host}:{sid}:{op_id}））
	sid := req.SessionID
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
// required = 声明表 level 与 vcore 动态表（git/browser 子命令、rm -r 非空提升）取高。
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
		if decl, ok := c.cmdByName[p.Action]; ok {
			required = decl.RequiredLevel
			if dyn := vcore.ExecRequiredIn(c.newEnv(""), p.Action, p.Argv); dyn > required {
				required = dyn
			}
		}
		// 未声明命令按 Danger 兜底（后续路由会拒绝，这里只是纵深检查的保守值）
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

// execCmd 执行 exec 请求（§5.1 统一命令声明模型）：
// 按声明表路由——核心虚拟指令走 vcore.Run，browser/bg_* 走特化实现，
// 本地命令（探测声明的 shell/git）走 runLocal（exec_procs 托管）；
// 未声明命令一律拒绝（不存在「未知命令透传」）。
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
	if _, ok := c.cmdByName[p.Action]; !ok {
		return &proto.ToolResponse{MsgID: req.MsgID, State: proto.StateError,
			Error: fmt.Sprintf("exec: unknown action %q (not declared by this host; run commands to discover available commands)", p.Action)}
	}

	env := c.newEnv(p.Workdir)
	switch p.Action {
	case "commands":
		return &proto.ToolResponse{MsgID: req.MsgID, State: proto.StateCompleted,
			Content: c.commandsJSON(), Attrs: map[string]string{"action": "commands"}}
	case "browser":
		// §5.6 pod 模式：agent-browser CLI，不隔离（用户本机浏览器）
		return c.runBrowser(ctx, sid, req, p.Argv)
	case "bg_list":
		return resultToResponse(req.MsgID, c.bgList(sid), nil)
	case "bg_wait":
		res, err := c.bgWait(ctx, sid, p.Argv)
		return resultToResponse(req.MsgID, res, err)
	case "bg_kill":
		res, err := c.bgKill(sid, p.Argv)
		return resultToResponse(req.MsgID, res, err)
	}

	if isCoreCommand(p.Action) {
		res, err := vcore.Run(ctx, env, p.Action, p.Argv)
		return resultToResponse(req.MsgID, res, err)
	}

	// 本地命令（§5.9：探测声明的 shell/git）：PATH 查找、workdir = 进程 cwd、
	// 日志文件、deadline 超时自动后台化，统一经 exec_procs 托管
	// workdir 缺省回落：请求未携带时用 host 端配置工作区（与虚拟指令 newEnv 同语义）
	workdir := p.Workdir
	if workdir == "" {
		workdir = c.opts.WorkDir
	}
	return c.runLocal(ctx, sid, req.MsgID, p.Action, p.Argv, workdir)
}

// isCoreCommand 判定 action 是否为核心 8 虚拟指令（vcore 内存执行）。
func isCoreCommand(action string) bool {
	for _, n := range vcore.CoreCommandNames() {
		if n == action {
			return true
		}
	}
	return false
}

// commandsJSON 返回本 host 的命令表（§5.2：{name, desc} 视图——
// level 仅供审批判断，help 由服务端 procs 拦截 `-h` 返回，均不暴露给 AI）。
func (c *Client) commandsJSON() string {
	type item struct {
		Name string `json:"name"`
		Desc string `json:"desc"`
	}
	cmds := make([]item, 0, len(c.cmds))
	for _, d := range c.cmds {
		cmds = append(cmds, item{Name: d.Name, Desc: d.Desc})
	}
	data, _ := json.Marshal(map[string]any{"commands": cmds})
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
