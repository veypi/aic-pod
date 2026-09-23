package host

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/veypi/aic-pod/libs/exec_procs"
	"github.com/veypi/aic-pod/libs/fsx"
	tool "github.com/veypi/aic-pod/libs/hosts_tool"
	"github.com/veypi/aic-pod/libs/proto"
	vshglue "github.com/veypi/aic-pod/libs/vsh"
	wire "github.com/veypi/aic-pod/protocol/hosts_tools"
)

// registerCommands 注册 exec 的 wire 面（vsh 引擎化，todo 3.1.3）：唯一命令
// "exec" + 唯一方法 run（{script, workdir?, timeout?, stdin?, nosandbox?}）。
// 旧逐名命令注册（curl/git/json/bg_*/shell 逃生舱）随 vcore 删除退役——
// 命令发现 = 脚本内 `commands`；后台 = 脚本内 `bg`；授权 = 脚本内 `grant`。
//
// 等级（正交模型）：基线 Write(2)（规则表内操作不审批）；script 含字面
// grant 或 nosandbox → Critical(4)（dispatcher 经 Required 强制，与 aic 侧
// 审批同源纵深）。
func (c *Client) registerCommands() {
	run := tool.Bind(tool.Spec{Name: "run", Access: 2, Description: "run a vsh shell script"}, func(ctx context.Context, caller tool.Caller, a execScriptParams) (any, error) {
		data, _ := json.Marshal(a)
		req := &proto.ToolRequest{Tool: proto.ToolExec, MsgID: caller.RequestID, SessionID: caller.Origin, GrantedLevel: caller.Level, Data: data}
		response := c.execCmd(ctx, caller.Origin, req)
		if response.Error != "" {
			return nil, wire.Fail(string(response.State), response.Error)
		}
		return &fsx.Result{Content: response.Content, Attrs: response.Attrs}, nil
	})
	run.Required = func(raw json.RawMessage) int {
		var a execScriptParams
		_ = json.Unmarshal(raw, &a)
		level := 2
		if a.NoSandbox || len(vshglue.Analyze(a.Script, nil).GrantRequests) > 0 {
			level = 4
		}
		return level
	}
	if err := c.tools.RegisterCommand(tool.Command{
		Name: "exec", Desc: "run a vsh shell script on this host",
		Help: "exec run {script, workdir?, timeout?, stdin?, nosandbox?}\n" +
			"  Execute a vsh shell script (90 builtins: ls/rg/cp/mv/rm/cat/jq/tar/sed/awk...).\n" +
			"  Discovery: run `commands` in a script; usage: `<cmd> --help`.\n" +
			"  Background: `bg` in a script; grants: `grant fs|net|cmd <target>` (level 4).",
		Methods: []tool.Method{run},
	}); err != nil {
		panic(err)
	}
}

func executionOwner(c tool.Caller) string { return c.Subject + "\x00" + c.Origin }

// executeCommand 是 wire 执行准入（hosts_tool Execute 回调）：epoch 校验 +
// 长任务托管（Background 方法经 exec_procs 幂等去重/断线续跑）。控制方法
// （Background=false，exec.run 即此类——bg 由引擎任务表承接）直接调用。
func (c *Client) executeCommand(ctx context.Context, caller tool.Caller, r wire.Request, in wire.Invocation, m tool.Method) (any, error) {
	if r.Execution != nil && r.Execution.Epoch != c.procs.Epoch() {
		return nil, wire.Fail("expired", "Execution belongs to a different device runtime")
	}
	// 控制方法（exec.run/cua/browser 的短调用）不产生执行记录，直接调用。
	if !m.Descriptor.Background {
		return m.Run(ctx, caller, in.Args)
	}
	// 等待上限 = 请求 timeout_ms（服务端已按硬上限钳位）。到点未完成即返回执行记录
	// （background=true + id），执行继续运行——运行预算由执行管理器自有超时决定，
	// 不受本次等待影响。
	waitMS := r.TimeoutMS
	if waitMS <= 0 {
		waitMS = 30000
	}
	id := r.ID
	if r.Execution != nil && r.Execution.ID != "" {
		id = r.Execution.ID
	}
	owner := executionOwner(caller)
	hash := sha256.Sum256([]byte(owner))
	// Ownership is independent of RTC connections and is safe as a path segment.
	namespace := fmt.Sprintf("%x", hash[:16])
	fullID := c.hostID + ":" + namespace + ":" + id
	logPath := c.execLogPath(namespace, id)
	// Capture this admission's authority. A new connection never extends it.
	runCaller := caller
	runCaller.Expiry = nil
	runCaller.Check = nil
	if caller.AllowStreams {
		runCaller.ExpiresAt = time.Now().Add(c.options().ExecTimeout)
	}
	runCaller.Check = func(run context.Context) error {
		if !c.execAllowed(runCaller.Origin, in.Command) {
			return wire.Fail("permission_denied", "Command authorization revoked")
		}
		return nil
	}
	// 幂等去重只绑定调用内容与授权等级：等待时长可以改变，执行内容不能改变。
	digestRaw, _ := json.Marshal(struct {
		Call  wire.Invocation
		Level int
	}{in, caller.Level})
	digest := sha256.Sum256(digestRaw)
	wait, cancel := context.WithTimeout(ctx, time.Duration(waitMS)*time.Millisecond)
	defer cancel()
	required := m.Descriptor.Access
	if m.Required != nil {
		required = max(required, m.Required(in.Args))
	}
	res, err := c.procs.StartCall(wait, exec_procs.CallOptions{RequiredLevel: required, ID: fullID, Owner: owner, Digest: fmt.Sprintf("%x", digest), Command: in.Command + " " + in.Method, LogPath: logPath, AuthorizationDeadline: runCaller.ExpiresAt, Check: runCaller.Validate, Run: func(run context.Context, out io.Writer) (any, error) {
		runCaller.Output = out
		value, err := m.Run(run, runCaller, in.Args)
		if value != nil {
			if result, ok := value.(*fsx.Result); ok {
				if result.Content != "" {
					fmt.Fprintln(out, result.Content)
				}
			} else {
				raw, _ := json.Marshal(value)
				if len(raw) < wire.MaxMessageBytes/2 {
					fmt.Fprintln(out, string(raw))
				}
			}
		}
		return value, err
	}})
	if err != nil {
		return nil, err
	}
	return res, nil
}
