package host

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/exec_procs"
	"github.com/veypi/aic-pod/libs/proto"
	"github.com/veypi/aic-pod/libs/vcore"
	"github.com/veypi/aic-pod/protocol/ui"
)

// handleMsg 处理一条入站消息：rtc.in 信令路由到 RTC 服务（不参与验签流程——
// 信令身份由 NATS 权限模型保证，DataChannel 另有 code 鉴权帧）；
// 其余按工具请求处理（§6.2 host 端验证规范）：
// 验签 → deadline 过期拒绝 → nonce 窗口去重 → granted_level 纵深检查 → 分发。
func (c *Client) handleMsg(msg *nats.Msg) {
	if strings.HasSuffix(msg.Subject, ".rtc.in") {
		c.handleRTCSignal(msg.Data)
		return
	}
	resp := c.dispatch(context.Background(), msg.Subject, msg.Data)
	if resp == nil {
		return
	}
	data, _ := json.Marshal(resp)
	msg.Respond(data)
	c.logf("← response: msg=%s state=%s error=%q", resp.MsgID, resp.State, resp.Error)
}

// dispatch 是请求处理主流程（与 NATS 解耦，可单测）。
func (c *Client) dispatch(ctx context.Context, subject string, data []byte) (response *proto.ToolResponse) {
	var uiDomain string
	var uiArgv []string
	defer func() {
		if response != nil && response.State == proto.StateWaiting {
			response.State = proto.StateRejected
			if response.NeedApproval != nil {
				response.Error = response.NeedApproval.Reason
			}
			response.NeedApproval = nil
		}
		if response != nil && uiDomain != "" {
			normalizeUIResponse(response, uiDomain, uiArgv)
		}
	}()
	var req proto.ToolRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return &proto.ToolResponse{State: proto.StateError, Error: "invalid request: " + err.Error()}
	}
	c.logf("→ request: %s %s (msg=%s)", req.Tool, subject, req.MsgID)

	// 1. 验签
	if req.Sig == "" || !proto.VerifyToolRequest(&req, c.hostID, c.kTool) {
		return reject(req.MsgID, "invalid request signature")
	}

	if req.Tool == proto.ToolExec {
		var p struct {
			Action string   `json:"action"`
			Argv   []string `json:"argv"`
		}
		if json.Unmarshal(req.Data, &p) == nil && (p.Action == "browser" || p.Action == "cua") {
			uiDomain, uiArgv = p.Action, p.Argv
			if _, err := ui.Parse(p.Action, p.Argv); err != nil {
				return uiFailure(req.MsgID, p.Action, p.Argv, err, "error")
			}
		}
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

	// Server-only lifecycle command: ordinary tool discovery never advertises it.
	if req.Tool == proto.ToolExec && req.SessionID != "" && req.GrantedLevel == proto.LevelApproved && actionOf(&req) == "_session_end" {
		c.dropSessionGrants(req.SessionID)
		cleanup, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		var cleanupErrors []string
		if err := cuaUI.endSession(cleanup, req.SessionID); err != nil {
			cleanupErrors = append(cleanupErrors, err.Error())
		}
		if p, ok := lookupProvider("browser"); ok && p.EndSession != nil {
			if err := p.EndSession(cleanup, req.SessionID); err != nil {
				cleanupErrors = append(cleanupErrors, err.Error())
			}
		}
		if len(cleanupErrors) > 0 {
			return &proto.ToolResponse{MsgID: req.MsgID, State: proto.StateError, Error: strings.Join(cleanupErrors, "; ")}
		}
		return &proto.ToolResponse{MsgID: req.MsgID, State: proto.StateCompleted, Content: "session grants and UI bindings cleared"}
	}
	// 4. granted_level 纵深检查（§2.4 判定分工：host 端按 caps 声明 + 本地规则再自检）
	//    校验不通过直接拒绝；host 执行策略不返回提权请求。
	if state, reason := c.checkGranted(&req); state != "" {
		resp := &proto.ToolResponse{MsgID: req.MsgID, State: state}
		if state == proto.StateWaiting {
			resp.NeedApproval = &proto.NeedApproval{Reason: reason}
		} else {
			resp.Error = reason
		}
		return resp
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
		if uiDomain != "" {
			return c.runUIOnce(ctx, &req, uiDomain, uiArgv, func() *proto.ToolResponse { return c.execCmd(ctx, sid, &req) })
		}
		return c.execCmd(ctx, sid, &req)
	}
	return &proto.ToolResponse{MsgID: req.MsgID, State: proto.StateError,
		Error: fmt.Sprintf("unknown tool %q (supported: fs, exec)", req.Tool)}
}

// checkGranted 做 granted >= required 数字比较（与 vcore 分级表同源）。
// required = 声明表 level 与 vcore 动态表（git/browser 子命令、fs rm recursive
// 删非空目录提升）取高。browser 由壳 provider 注册时才在声明表出现（register.go）。
// 不足返回 rejected；用户通过单独的 grant 调用申请本地授权。
func (c *Client) checkGranted(req *proto.ToolRequest) (proto.State, string) {
	// 0 = 显式禁用：直接拒绝，不可审批绕过（与服务端 procs 同语义，纵深防御）。
	if req.GrantedLevel == proto.LevelNone {
		return proto.StateRejected, "tool is explicitly denied (level 0)"
	}
	required := proto.LevelDanger
	switch req.Tool {
	case proto.ToolFS:
		var p struct {
			Action string `json:"action"`
		}
		_ = json.Unmarshal(req.Data, &p)
		required = vcore.FSRequiredIn(c.newEnv("", ""), p.Action, req.Data)
	case proto.ToolExec:
		var p struct {
			Action    string   `json:"action"`
			Argv      []string `json:"argv"`
			NoSandbox bool     `json:"nosandbox"`
		}
		_ = json.Unmarshal(req.Data, &p)
		c.cmdsMu.RLock()
		decl, ok := c.cmdByName[p.Action]
		c.cmdsMu.RUnlock()
		if ok {
			required = decl.RequiredLevel
			if dyn := vcore.ExecRequired(p.Action, p.Argv); dyn > required {
				required = dyn
			}
		}
		// nosandbox 免沙箱请求：required 下限 Critical(4)（用户不可授予 ⇒ 必审批；
		// 审批放行后 granted=9 + nosandbox 标记随行下发，exec_procs 仅据此标记
		// 免沙箱——审批本身（9 无标记）仍沙箱执行，§5.10）
		if p.NoSandbox && required < proto.LevelCritical {
			required = proto.LevelCritical
		}
		// 未声明命令按 Danger 兜底（后续路由会拒绝，这里只是纵深检查的保守值）
	}
	if req.GrantedLevel < required {
		return proto.StateRejected, fmt.Sprintf("%s %s requires level %d (granted %d)",
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

// execFS 执行 fs 请求（vcore + OS VFS 适配 + 文件权限模型判定）。
func (c *Client) execFS(ctx context.Context, req *proto.ToolRequest) *proto.ToolResponse {
	env := c.newEnv(req.SessionID, "")
	env.Granted = req.GrantedLevel
	res, err := vcore.RunFS(ctx, env, req.Data)
	return resultToResponse(req.MsgID, res, err)
}

// newEnv 构建 OS 文件系统执行环境。
// workdir 为空时使用 host 端配置工作区（§2.1.1 缺省值）；sid 绑定文件权限
// 视图的会话上下文（临时 grant/会话区，v0.14.5 §2）——checkGranted 的
// FSRequiredIn 探测传空 sid（仅 Resolve/VFS，不触发策略判定）；
// 有文件副作用的指令（provider 转发等）传真实 sid（deny/grant 判定需要会话上下文）。
func (c *Client) newEnv(sid, workdir string) *vcore.Env {
	if workdir == "" {
		workdir = c.opts.WorkDir
	}
	return &vcore.Env{
		VFS:          OSVFS{},
		Workdir:      workdir,
		ProtectRoots: filesystemRoots(),
		VirtualRoot:  runtime.GOOS == "windows",        // windows "/" = 盘符挂载列表（虚拟根）
		Fetcher:      shellCurlFetcher{c: c, sid: sid}, // 外部 http(s) 走真 curl + 统一沙箱（net 域出站闸）
		ImageData:    true,                             // host 端图片经 image_data 返回（§2.2）
		Policy:       c.policy.View(sid),
	}
}

// execCmd 执行 exec 请求（§5.1 统一命令声明模型）：
// 按声明表路由——核心虚拟指令走 vcore.Run，bg_*/grant/ssh/scp 走特化实现，
// 本地命令（探测声明的 shell/git）走 runLocal（exec_procs 托管），壳注册命令
// （browser 等）走 provider 转发；未声明命令一律拒绝（不存在「未知命令透传」）。
func (c *Client) execCmd(ctx context.Context, sid string, req *proto.ToolRequest) *proto.ToolResponse {
	var p struct {
		Action    string   `json:"action"`
		Argv      []string `json:"argv"`
		Workdir   string   `json:"workdir"`
		NoSandbox bool     `json:"nosandbox"`
	}
	if err := json.Unmarshal(req.Data, &p); err != nil {
		return &proto.ToolResponse{MsgID: req.MsgID, State: proto.StateError, Error: "invalid exec data: " + err.Error()}
	}
	if p.Action == "" {
		return &proto.ToolResponse{MsgID: req.MsgID, State: proto.StateError,
			Error: "exec: action is required"}
	}
	c.cmdsMu.RLock()
	_, declared := c.cmdByName[p.Action]
	c.cmdsMu.RUnlock()
	if !declared {
		return &proto.ToolResponse{MsgID: req.MsgID, State: proto.StateError,
			Error: fmt.Sprintf("exec: unknown action %q (not declared by this host; run commands to discover available commands)", p.Action)}
	}

	if !c.execAllowed(sid, p.Action) {
		return reject(req.MsgID, "exec "+p.Action+" is denied by host exec policy; request access with grant exec "+p.Action+" --temp")
	}
	env := c.newEnv(sid, p.Workdir)
	env.Granted = req.GrantedLevel // Policy 升级判定用（v0.14.5 §2）
	// 任务托管（curl 无 -o）：输出落盘 {tmp}/aic/{sid}/.exec/{msg_id}.log，
	// 超时自动后台化（与本地命令同一 exec_procs 机制，§5.9）。
	env.Tasks = &hostTaskRunner{c: c, sid: sid}
	env.TaskID = req.MsgID
	if p.Action == "browser" || p.Action == "cua" {
		operation, err := ui.Parse(p.Action, p.Argv)
		if err != nil {
			return uiFailure(req.MsgID, p.Action, p.Argv, err, "error")
		}
		if operation.Op == "run" {
			return c.runUIScript(ctx, sid, req, operation, p.Workdir)
		}
	}
	switch p.Action {
	case "commands":
		return &proto.ToolResponse{MsgID: req.MsgID, State: proto.StateCompleted,
			Content: c.commandsJSON(), Attrs: map[string]string{"action": "commands"}}
	case "json":
		// json 虚拟指令（vcore 内存实现）：view/set/del/append/merge
		res, err := vcore.Run(ctx, env, p.Action, p.Argv)
		return resultToResponse(req.MsgID, res, err)
	case "bg_list":
		return resultToResponse(req.MsgID, c.bgList(sid), nil)
	case "bg_wait":
		res, err := c.bgWait(ctx, sid, p.Argv)
		return resultToResponse(req.MsgID, res, err)
	case "bg_kill":
		res, err := c.bgKill(sid, p.Argv)
		return resultToResponse(req.MsgID, res, err)
	case "grant":
		// 统一授权申请（fs/net/ssh 三域；required 4 必审批在 checkGranted 门控）。
		return c.runGrant(sid, req.MsgID, p.Argv)
	case "ssh":
		// ssh 一级工具（独立通道：目标闸 = ssh 域 Policy；免沙箱内置执行）
		return c.runSSH(ctx, sid, req, p.Argv)
	case "scp":
		// scp 一级工具（目标闸同 ssh 域；本地侧过 fsauth 门控；免沙箱内置执行）
		return c.runSCP(ctx, sid, req, p.Argv)
	case "cua":
		// cua 一级命令（§5.10：Go 原生桥接 cua-driver MCP 持久子进程）
		return c.runCua(ctx, sid, req, p.Argv)
	}

	if p.Action == "browser" {
		operation, err := ui.Parse("browser", p.Argv)
		if err != nil {
			return uiFailure(req.MsgID, "browser", p.Argv, err, "error")
		}
		if operation.Op == "upload" {
			abs, err := env.Resolve(operation.String("file"))
			if err == nil {
				err = env.CheckPath("browser upload", abs)
			}
			if err == nil {
				err = env.CheckPolicy("browser upload", abs, false)
			}
			if err == nil {
				var info os.FileInfo
				info, err = os.Stat(abs)
				if err == nil && !info.Mode().IsRegular() {
					err = fmt.Errorf("upload requires a regular file")
				}
			}
			if err != nil {
				return uiFailure(req.MsgID, "browser", p.Argv, err, "rejected")
			}
			for i := 0; i+1 < len(p.Argv); i++ {
				if p.Argv[i] == "--file" {
					p.Argv[i+1] = abs
					break
				}
			}
		}
		if operation.Op == "download" {
			if err := env.CheckPolicy("browser download", filepath.Join(sessionWorkDir(sid), ".browser"), true); err != nil {
				return uiFailure(req.MsgID, "browser", p.Argv, err, "rejected")
			}
		}
	}

	// 壳 provider 命令（desktop browser 等，register.go）：转发壳进程执行
	if prov, ok := lookupProvider(p.Action); ok {
		return prov.Run(ctx, sid, req, p.Argv)
	}

	if isCoreCommand(p.Action) {
		res, err := vcore.Run(ctx, env, p.Action, p.Argv)
		return resultToResponse(req.MsgID, res, err)
	}

	// 本地命令（§5.9：探测声明的 shell/git）：PATH 查找、workdir = 进程 cwd、
	// 日志文件、deadline 超时自动后台化，统一经 exec_procs 托管；
	// granted level 与 nosandbox 随行传给 exec_procs（§5.10：沙箱去留只由
	// 显式 nosandbox 决定——审批通过（9）不豁免沙箱）
	// workdir 缺省回落：请求未携带时用 host 端配置工作区（与虚拟指令 newEnv 同语义）
	workdir := p.Workdir
	if workdir == "" {
		workdir = c.opts.WorkDir
	}
	return c.runLocal(ctx, sid, req.MsgID, p.Action, p.Argv, workdir, req.GrantedLevel, p.NoSandbox)
}

// isCoreCommand 判定 action 是否为 exec 核心虚拟指令（vcore 内存执行）。
// 文件类指令（ls/rg/cp/mv/rm）属 fs 指令集，不在此列。
func isCoreCommand(action string) bool {
	for _, n := range vcore.CoreCommandNames() {
		if n == action {
			return true
		}
	}
	return false
}

// sessionWorkDir 返回会话工作区（v0.14.5 §4 布局，两端同构 UserOutputDir/sessions/{sid}）：
// $HOME/.aic/sessions/{sid}——exec 日志（.exec/）、壳 provider 文件交换（.browser/）、
// 截图（.screenshot/）的落点。PublicDir 不可得时回落系统临时目录旧位
// （{tmp}/aic/{sid}，临时产物语义不变）。
func sessionWorkDir(sid string) string {
	if dir, err := cfg.PublicDir(); err == nil {
		return filepath.Join(dir, "sessions", sid)
	}
	return filepath.Join(os.TempDir(), "aic", sid)
}

// hostTaskRunner 实现 vcore.TaskRunner：托管任务（curl 无 -o）经 exec_procs
// 统一托管，输出落盘 {tmp}/aic/{sid}/.exec/{msg_id}.log（与本地命令同一机制，§5.9）。
type hostTaskRunner struct {
	c   *Client
	sid string
}

func (r *hostTaskRunner) StartTask(ctx context.Context, opts vcore.TaskOptions) (*vcore.TaskResult, error) {
	id := fmt.Sprintf("%s:%s:%s", r.c.hostID, r.sid, opts.ID)
	logPath := filepath.Join(sessionWorkDir(r.sid), ".exec", opts.ID+".log")
	res, err := r.c.procs.StartTask(ctx, exec_procs.TaskOptions{
		ID:      id,
		Command: opts.Command,
		LogPath: logPath,
		Run:     opts.Run,
	})
	if err != nil {
		return nil, err
	}
	return &vcore.TaskResult{
		Content:    res.Content,
		Lines:      res.Lines,
		Truncated:  res.Truncated,
		Background: res.Background,
		ID:         res.ID,
		LogPath:    res.LogPath,
	}, nil
}

// commandsJSON 返回本 host 的命令表（§5.2：{name, desc} 视图——
// level 仅供审批判断，help 由服务端 procs 拦截 `-h` 返回，均不暴露给 AI）。
func (c *Client) commandsJSON() string {
	type item struct {
		Name string `json:"name"`
		Desc string `json:"desc"`
	}
	c.cmdsMu.RLock()
	cmds := make([]item, 0, len(c.cmds))
	for _, d := range c.cmds {
		cmds = append(cmds, item{Name: d.Name, Desc: d.Desc})
	}
	c.cmdsMu.RUnlock()
	data, _ := json.Marshal(map[string]any{"commands": cmds})
	return string(data)
}

// resultToResponse 将 vcore.Result/错误映射为响应信封（§6.2 错误模型）。
func resultToResponse(msgID string, res *vcore.Result, err error) *proto.ToolResponse {
	if err != nil {
		state := proto.StateOf(err)
		if state == proto.StateWaiting {
			state = proto.StateRejected
		}
		resp := &proto.ToolResponse{MsgID: msgID, State: state, Error: err.Error()}

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
