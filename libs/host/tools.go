package host

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/veypi/aic-pod/protocol"

	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/veypi/aic-pod/libs/execution"
	"github.com/veypi/aic-pod/libs/fsx"
)

func (c *Client) initTools() {
	if c.initErr = c.initFilesystem(); c.initErr != nil {
		return
	}
	reg, err := execution.NewRegistry()
	if err != nil {
		c.initErr = err
		return
	}
	c.initErr = c.configureMCP(c.options().MCP)
	if c.initErr != nil {
		return
	}
	c.vsh.engine, c.initErr = c.buildVSHEngine(reg)
	if c.initErr != nil {
		return
	}
}

// DisconnectTools 断连清理：取消该连接发起的前台执行（后台任务继续，
// 其取消显式走 cancel/bg kill；stream 随 RTC 通道关闭由对端清理）。
func (c *Client) DisconnectTools(caller protocol.Caller) {
	if engine, err := c.engine(); err == nil {
		engine.Tasks.CancelConnection(caller.ConnectionID)
	}
}

func (c *Client) handleToolsMsg(msg *nats.Msg) {
	response := c.HandleNATS(context.Background(), msg.Subject, msg.Data)
	raw, err := json.Marshal(response)
	if err == nil {
		err = msg.Respond(raw)
	}
	if err != nil {
		c.logf("Tool request: %v", err)
	}
}

func (c *Client) handleFS(ctx context.Context, caller protocol.Caller, in *protocol.FSInvocation) (any, error) {
	if c.perms.err() != nil {
		return nil, protocol.Fail("permission_denied", "Device authorization configuration is invalid; repair local settings")
	}
	if strings.HasPrefix(in.Method, "text.") {
		action := strings.TrimPrefix(in.Method, "text.")
		var params map[string]any
		if err := protocol.Decode(in.Args, &params); err != nil {
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
		// FS 层失败按需归类（not_found / permission_denied / filesystem_error），
		// 其余交给 AsFault（internal 等）。
		return result, fsFault(err)
	}
	return c.files.Handle(ctx, caller, in.Method, in.Args)
}

func (c *Client) dispatch(ctx context.Context, caller protocol.Caller, r protocol.Request) protocol.Response {
	if err := r.Validate(); err != nil {
		return protocol.Reply(r.Protocol, r.ID, nil, err)
	}
	if err := caller.Validate(ctx); err != nil {
		return protocol.Reply(r.Protocol, r.ID, nil, err)
	}
	// 授权配置损坏 fail-closed（修复本地设置前拒绝一切执行/文件请求）。
	if c.perms.err() != nil {
		return protocol.Reply(r.Protocol, r.ID, nil, protocol.Fail("permission_denied", "Device authorization configuration is invalid; repair local settings"))
	}
	// 文件代理原有 fs-only 范围继续有效：不能借文件入口执行命令。
	if caller.Scope == "fs" && r.Action != protocol.ActionFS {
		return protocol.Reply(r.Protocol, r.ID, nil, protocol.Fail("permission_denied", "File proxy only permits fs"))
	}
	var v any
	var err error
	switch r.Action {
	case protocol.ActionExec:
		if r.Exec.NoSandbox && !caller.GrantApproved {
			return protocol.Reply(r.Protocol, r.ID, nil, protocol.Fail("permission_denied", "Unsandboxed execution requires approval"))
		}
		v, err = c.execScript(ctx, caller, r.ID, r.Exec)
	case protocol.ActionFS:
		v, err = c.handleFS(ctx, caller, r.FS)
	case protocol.ActionCancel:
		v, err = c.cancelExec(caller, r.CancelID)
	default:
		err = protocol.Fail("unsupported", "Unknown transport action")
	}
	return protocol.Reply(r.Protocol, r.ID, v, err)
}

// HandleNATS 是 NATS 工具请求入口（hosts_nats/2 可信转发）：验签 → 身份 →
// nonce 去重 → 分发。granted_level 纵深检查已删除；grant_approved 随签名
// 信封进入可信调用上下文。
func (c *Client) HandleNATS(ctx context.Context, subject string, data []byte) protocol.Response {
	var r protocol.NatsRequest
	if err := protocol.Decode(data, &r); err != nil {
		return protocol.Reply(protocol.NatsProtocol, "", nil, err)
	}
	destination, err := protocol.NatsSubject(c.uid, c.hostID)
	if err != nil || subject != destination {
		return protocol.Reply(protocol.NatsProtocol, r.Request.ID, nil, protocol.Fail("unauthorized", "Wrong tool route"))
	}
	platformNow, localNow := clockNow(), time.Now()
	if err = protocol.NatsVerify(c.kTool, c.hostID, subject, r, platformNow); err != nil {
		return protocol.Reply(protocol.NatsProtocol, r.Request.ID, nil, err)
	}
	if r.Caller != c.uid {
		return protocol.Reply(protocol.NatsProtocol, r.Request.ID, nil, protocol.Fail("permission_denied", "Caller does not own this device"))
	}
	deadline := localNow.Add(time.UnixMilli(r.Deadline).Sub(platformNow))
	if !c.replay.checkAndMark("tools:"+r.Nonce, deadline) {
		return protocol.Reply(protocol.NatsProtocol, r.Request.ID, nil, protocol.Fail("unauthorized", "Duplicate nonce"))
	}
	// Origin 是会话归属的签名元数据；GrantApproved 是服务端审批事实。
	caller := protocol.Caller{Subject: r.Caller, ConnectionID: "nats:" + r.Caller, Origin: r.Origin, Scope: r.Scope, GrantApproved: r.GrantApproved, ExpiresAt: localNow.Add(time.UnixMilli(r.AuthorizationUntil).Sub(platformNow))}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	return c.dispatch(ctx, caller, r.Request)
}

// HandleTool 是 RTC 普通请求入口（与 NATS 同一分发实现）。
func (c *Client) HandleTool(ctx context.Context, caller protocol.Caller, r protocol.Request) protocol.Response {
	return c.dispatch(ctx, caller, r)
}

func (c *Client) cancelExec(caller protocol.Caller, cancelID string) (any, error) {
	// 唯一运行记录表：前台与 bg 同一记录（bg 置 killed 终态）。
	// 归属不匹配或已终结等同不存在。
	engine, err := c.engine()
	if err != nil {
		return nil, protocol.Fail("internal", "exec: engine: "+err.Error())
	}
	if err := engine.Tasks.CancelByRequest(cancelID, caller.Subject, caller.Origin); err != nil {
		if errors.Is(err, execution.ErrNoRun) {
			return nil, protocol.Fail("not_found", "No active execution")
		}
		return nil, err
	}
	return map[string]bool{"cancel_requested": true}, nil
}
