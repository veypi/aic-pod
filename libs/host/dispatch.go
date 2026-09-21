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
	tool "github.com/veypi/aic-pod/libs/hosts_tool"
	"github.com/veypi/aic-pod/libs/proto"
	"github.com/veypi/aic-pod/libs/vcore"
)

// handleMsg 处理一条入站消息：rtc.in 信令路由到 RTC 服务（不参与验签流程——
// 信令身份由 NATS 权限模型保证，DataChannel 使用绑定 DTLS 证书的短期票据）；
// 其余按工具请求处理（§6.2 host 端验证规范）：
// 验签 → deadline 过期拒绝 → nonce 窗口去重 → granted_level 纵深检查 → 分发。
func (c *Client) handleMsg(msg *nats.Msg) {
	if strings.HasSuffix(msg.Subject, ".tools.req") {
		c.handleToolsMsg(msg)
		return
	}
	if strings.HasSuffix(msg.Subject, ".rtc.in") {
		c.handleRTCSignal(msg.Data)
	}
}

// newEnv 构建 OS 文件系统执行环境。
// workdir 为空时使用 host 端配置工作区（§2.1.1 缺省值）；sid 绑定文件权限
// 视图的会话上下文（临时 grant/会话区，v0.14.5 §2）——checkGranted 的
// FSRequiredIn 探测传空 sid（仅 Resolve/VFS，不触发策略判定）；
// 有文件副作用的指令（provider 转发等）传真实 sid（deny/grant 判定需要会话上下文）。
func (c *Client) newEnv(sid, workdir string) *vcore.Env {
	if workdir == "" {
		workdir = c.options().WorkDir
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

// execCmd implements registered raw-argv commands after common admission.
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
	declared := c.tools.HasCommand(p.Action)
	if !declared {
		return &proto.ToolResponse{MsgID: req.MsgID, State: proto.StateError,
			Error: fmt.Sprintf("exec: unknown action %q (not declared by this host; run commands to discover available commands)", p.Action)}
	}

	if !c.execAllowed(sid, p.Action) {
		return reject(req.MsgID, "exec "+p.Action+" is denied by host exec policy; request access with grant exec "+p.Action+" --temp")
	}
	env := c.newEnv(sid, p.Workdir)
	env.Granted = req.GrantedLevel
	if c.files != nil {
		env.VFS = c.files.View(ctx, tool.Caller{Subject: c.uid, Origin: sid, Level: req.GrantedLevel})
	}
	// 任务托管（curl 无 -o）：输出落盘 {tmp}/aic/{sid}/.exec/{msg_id}.log，
	// 超时自动后台化（与本地命令同一 exec_procs 机制，§5.9）。
	env.Tasks = &hostTaskRunner{c: c, sid: sid}
	env.TaskID = req.MsgID
	switch p.Action {
	case "commands":
		return &proto.ToolResponse{MsgID: req.MsgID, State: proto.StateCompleted,
			Content: c.commandsJSON(), Attrs: map[string]string{"action": "commands"}}
	case "json":
		// json 虚拟指令（vcore 内存实现）：view/set/del/append/merge
		res, err := vcore.Run(ctx, env, p.Action, p.Argv)
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
		workdir = c.options().WorkDir
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

// sessionWorkDir allocates execution output under a trusted source namespace.
func (c *Client) sessionWorkDir(sid string) string {
	if c.sessionRoot != "" {
		return filepath.Join(c.sessionRoot, sid)
	}
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
	if out := exec_procs.Output(ctx); out != nil {
		return &vcore.TaskResult{}, opts.Run(ctx, out)
	}
	id := fmt.Sprintf("%s:%s:%s", r.c.hostID, r.sid, opts.ID)
	logPath := filepath.Join(r.c.sessionWorkDir(r.sid), ".exec", opts.ID+".log")
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
	cmds := c.tools.Commands(context.Background(), tool.Caller{Subject: "catalog", ConnectionID: "catalog", Level: 9, ExpiresAt: time.Now().Add(time.Minute)})
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
