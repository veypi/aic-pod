package host

// tools.go 是 host 端请求分发（hosts-vsh-redesign §1/§4）：
// 认证通过的 NATS/RTC 请求就是服务端已允许执行的请求——pod 不重新分类
// 审批、不返回 waiting，只验证身份并按 action 分发：
//   - exec → vsh 引擎完整脚本（execScript，见 engine_vsh.go）
//   - fs   → FS 数据面直调（hostfs Handle + text.* 薄层）
//   - cancel → 取消一次执行（与 bg kill 共用执行句柄）
//
// 数字等级与命令目录（hosts_tool Dispatcher）已删除；权限在执行点看 rules。

import (
	"context"
	"encoding/json"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/execution"
	"github.com/veypi/aic-pod/libs/fsx"
	"github.com/veypi/aic-pod/libs/proto"
	"github.com/veypi/aic-pod/libs/skillrun"
	natswire "github.com/veypi/aic-pod/protocol/hosts_nats"
	wire "github.com/veypi/aic-pod/protocol/hosts_tools"
)

// execHandleEntry 是取消登记表的一项（前台执行按 request_id 登记；
// 转后台后同一句柄经任务表 RequestID 可达）。
type execHandleEntry struct {
	handle         *execution.ExecHandle
	owner, session string
	connectionID   string
}

func (c *Client) initTools() {
	if c.initErr = c.initFilesystem(); c.initErr != nil {
		return
	}
	dir, err := cfg.StateDir()
	if err != nil {
		c.initErr = err
		return
	}
	reg, err := execution.NewRegistry()
	if err != nil {
		c.initErr = err
		return
	}
	c.skills, c.initErr = skillrun.New(skillrun.Deps{SkillsDir: filepath.Join(dir, "skills"), RunDir: filepath.Join(dir, "run"), Manager: c.procs, Policy: c.nativePolicy, Workdir: proto.HostPathToOS, Registry: reg, Fetch: c.fetchSkillZip, Logf: c.logf})
	if c.initErr != nil {
		return
	}
	c.vsh.engine, c.initErr = c.buildVSHEngine(reg)
	if c.initErr != nil {
		return
	}
	c.skills.Rescan()
	c.skills.PreinstallEmbedded(context.Background())
}

// dispatch 是普通请求的统一分发入口（NATS 与 RTC 同载荷同分发，§4.3）。
// pod 不再运行审批分类函数，也不返回 waiting/approval_required。
func (c *Client) dispatch(ctx context.Context, caller wire.Caller, r wire.Request) wire.Response {
	if err := r.Validate(); err != nil {
		return wire.Reply(r.Protocol, r.ID, nil, err)
	}
	if err := caller.Validate(ctx); err != nil {
		return wire.Reply(r.Protocol, r.ID, nil, err)
	}
	// 授权配置损坏 fail-closed（修复本地设置前拒绝一切执行/文件请求）。
	if cfg.CheckAuth() != nil {
		return wire.Reply(r.Protocol, r.ID, nil, wire.Fail("permission_denied", "Device authorization configuration is invalid; repair local settings"))
	}
	// 文件代理原有 fs-only 范围继续有效：不能借文件入口执行命令。
	if caller.Scope == "fs" && r.Action != wire.ActionFS {
		return wire.Reply(r.Protocol, r.ID, nil, wire.Fail("permission_denied", "File proxy only permits fs"))
	}
	caller.RequestID = r.ID
	var v any
	var err error
	switch r.Action {
	case wire.ActionExec:
		v, err = c.execScript(ctx, caller, r.ID, r.Exec)
	case wire.ActionFS:
		v, err = c.handleFS(ctx, caller, r.FS)
	case wire.ActionCancel:
		v, err = c.cancelExec(caller, r.CancelID)
	default:
		err = wire.Fail("unsupported", "Unknown transport action")
	}
	return wire.Reply(r.Protocol, r.ID, v, err)
}

// HandleNATS 是 NATS 工具请求入口（hosts_nats/2 可信转发）：验签 → 身份 →
// nonce 去重 → 分发。granted_level 纵深检查已删除；grant_approved 随签名
// 信封进入可信调用上下文。
func (c *Client) HandleNATS(ctx context.Context, subject string, data []byte) wire.Response {
	var r natswire.Request
	if err := wire.Decode(data, &r); err != nil {
		return wire.Reply(natswire.Protocol, "", nil, err)
	}
	destination, err := natswire.Subject(c.uid, c.hostID)
	if err != nil || subject != destination {
		return wire.Reply(natswire.Protocol, r.Request.ID, nil, wire.Fail("unauthorized", "Wrong tool route"))
	}
	if err = natswire.Verify(c.kTool, c.hostID, subject, r, clockNow()); err != nil {
		return wire.Reply(natswire.Protocol, r.Request.ID, nil, err)
	}
	if r.Caller != c.uid {
		return wire.Reply(natswire.Protocol, r.Request.ID, nil, wire.Fail("permission_denied", "Caller does not own this device"))
	}
	deadline := time.UnixMilli(r.Deadline)
	if !c.replay.checkAndMark("tools:"+r.Nonce, deadline) {
		return wire.Reply(natswire.Protocol, r.Request.ID, nil, wire.Fail("unauthorized", "Duplicate nonce"))
	}
	// Origin 是会话归属的签名元数据；GrantApproved 是服务端审批事实。
	caller := wire.Caller{Subject: r.Caller, ConnectionID: "nats:" + r.Caller, Origin: r.Origin, Scope: r.Scope, GrantApproved: r.GrantApproved, ExpiresAt: time.UnixMilli(r.AuthorizationUntil)}
	return c.dispatch(ctx, caller, r.Request)
}

// HandleTool 是 RTC 普通请求入口（与 NATS 同一分发实现）。
func (c *Client) HandleTool(ctx context.Context, caller wire.Caller, r wire.Request) wire.Response {
	return c.dispatch(ctx, caller, r)
}

// OpenToolStream 是 RTC 私有 stream 端点表（§4.3）：不注册为 vsh 指令，不进
// commands/caps。端点解析全部走 skillrun.ResolveStreamEndpoint（v6 P5 泛化，
// browser 拆包后唯一路径）：
//   - {包名}.{流名}        → skill 包 manifest streams[]
//   - browser.page.frames/browser.page.input → browser 包声明的全名流名
//
// 权限门 = 解析出的包名经 execAllowed（与 vsh 指令同一判定：认证不是资源授权）。
// stream.open 负载 = 客户端 args 原样透传（端点参数由包自行解析校验，如 browser
// 的 PageArgs）。连接失效由 RTC 通道关闭传播（连接撤销 → 通道关闭 → 流关闭，
// 不做运行中规则复核——与 engine_vsh 文件头偏差 2 一致）。
func (c *Client) OpenToolStream(ctx context.Context, caller wire.Caller, endpoint string, args json.RawMessage) (wire.Stream, error) {
	if !caller.AllowStreams {
		return nil, wire.Fail("unsupported", "Streams require an RTC channel")
	}
	if err := caller.Validate(ctx); err != nil {
		return nil, err
	}
	pkg, stream, ok := c.skills.ResolveStreamEndpoint(endpoint)
	if !ok {
		return nil, wire.Fail("unsupported", "Unknown stream endpoint")
	}
	if !c.execAllowed(caller.Origin, pkg) {
		return nil, wire.Fail("permission_denied", pkg+": command denied by exec rules（grant cmd "+pkg+" 申请）")
	}
	s, err := c.skills.OpenStream(ctx, pkg, stream, args)
	if err != nil {
		return nil, err
	}
	return newSkillToolStream(s), nil
}

// DisconnectTools 断连清理：取消该连接发起的前台执行（后台任务的取消
// 显式走 cancel/bg kill；stream 随 RTC 通道关闭由对端清理）。
func (c *Client) DisconnectTools(caller wire.Caller) {
	var handles []*execution.ExecHandle
	c.execMu.Lock()
	for id, e := range c.execHandles {
		if e.connectionID == caller.ConnectionID {
			handles = append(handles, e.handle)
			delete(c.execHandles, id)
		}
	}
	c.execMu.Unlock()
	for _, h := range handles {
		h.Cancel()
	}
}

// handleToolsMsg 处理一条入站 NATS 工具消息。
func (c *Client) handleToolsMsg(msg *nats.Msg) {
	response := c.HandleNATS(context.Background(), msg.Subject, msg.Data)
	data, err := json.Marshal(response)
	if err != nil {
		return
	}
	_ = msg.Respond(data)
}

// --- cancel 登记表（cancel(request_id) 与 bg kill 共用执行句柄，§2.6） ---

// trackExec 登记前台执行的取消句柄（完成时经 watcher 移除；转后台时经
// untrackExec 摘除——后台执行的取消走任务表 Kill（RequestID 关联），
// DisconnectTools 不杀后台任务）。
func (c *Client) trackExec(requestID, owner, session, connectionID string, h *execution.ExecHandle) {
	c.execMu.Lock()
	c.execHandles[requestID] = &execHandleEntry{handle: h, owner: owner, session: session, connectionID: connectionID}
	c.execMu.Unlock()
	go func() {
		<-h.Done()
		c.execMu.Lock()
		delete(c.execHandles, requestID)
		c.execMu.Unlock()
	}()
}

// untrackExec 摘除取消登记（转后台时调用；幂等——watcher 的删除同为幂等）。
func (c *Client) untrackExec(requestID string) {
	c.execMu.Lock()
	delete(c.execHandles, requestID)
	c.execMu.Unlock()
}

// cancelExec 按 request_id 取消实际执行（§2.6：终止脚本及受管子进程，
// 不是只放弃等待；取消不回滚已发生的副作用）。归属不匹配等同不存在。
func (c *Client) cancelExec(caller wire.Caller, cancelID string) (any, error) {
	owner, session := caller.Subject, caller.Origin
	// 已转后台的执行走任务表 Kill（同一执行句柄，置 killed 终态）；
	// 前台未登记的执行用取消登记表。归属不匹配等同不存在。
	if engine, err := c.engine(); err == nil {
		if task, found := engine.Tasks.FindByRequest(cancelID, owner, session); found {
			if err := engine.Tasks.Kill(task.ID, owner, session); err != nil {
				return nil, err
			}
			return map[string]bool{"cancel_requested": true}, nil
		}
	}
	c.execMu.Lock()
	e, ok := c.execHandles[cancelID]
	c.execMu.Unlock()
	if ok && e.owner == owner && e.session == session {
		e.handle.Cancel()
		return map[string]bool{"cancel_requested": true}, nil
	}
	return nil, wire.Fail("not_found", "No active execution")
}

// --- FS 数据面（§4.4：FS 保持独立，不转成 shell 脚本） ---

// handleFS 直调现有 FS 服务；text.* 走 fsx 薄层（与 vsh 文件操作共用同一
// 份资源规则——fsGate 即规则表门）。
func (c *Client) handleFS(ctx context.Context, caller wire.Caller, in *wire.FSInvocation) (any, error) {
	if cfg.CheckAuth() != nil {
		return nil, wire.Fail("permission_denied", "Device authorization configuration is invalid; repair local settings")
	}
	if strings.HasPrefix(in.Method, "text.") {
		action := strings.TrimPrefix(in.Method, "text.")
		var params map[string]any
		if err := wire.Decode(in.Args, &params); err != nil {
			return nil, err
		}
		params["action"] = action
		raw, _ := json.Marshal(params)
		env := &fsx.Env{
			FS:          c.files.View(ctx, caller),
			Workdir:     filepath.ToSlash(c.options().WorkDir),
			ImageData:   true, // host 端图片经 image_data 返回（§2.2）
			VirtualRoot: runtime.GOOS == "windows",
			Gate:        c.fsGate(caller.Origin),
		}
		result, err := fsx.RunFS(ctx, env, raw)
		if result != nil {
			c.attachFileURL(result.Attrs)
		}
		return result, err
	}
	return c.files.Handle(ctx, caller, in.Method, in.Args)
}
