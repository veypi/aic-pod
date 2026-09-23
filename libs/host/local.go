package host

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/veypi/aic-pod/libs/exec_procs"
	"github.com/veypi/aic-pod/libs/fsauth"
	"github.com/veypi/aic-pod/libs/proto"
)

// 本地命令执行（§5.9：探测声明的 shell/git）：stdout+stderr 合并写入
// {tmp}/aic/{session_id}/.exec/{msg_id}.log（与 cloud 端会话空间 .exec/ 同构）；请求 deadline 内完成 → 返回日志前 1000 行；
// 到期未完成 → 自动转后台（host 端自有超时，默认 30m），
// 返回当前前 1000 行 + background=true + bg id（{host}:{sid}:{op_id}）。
// argv 数组直传，禁止拼接 shell 字符串（§5.5：用户输入不经 shell 解释，杜绝注入）。

// runLocal 执行本地命令（§5.9）：子进程经 exec_procs 统一托管；
// level 为本次调用的授予等级（沙箱 profile 选择，§5.10）；
// noSandbox 为请求显式携带的免沙箱标记（已过 checkGranted 的 Critical(4) 必审批门）。
func (c *Client) runLocal(ctx context.Context, sid, msgID, action string, argv []string, workdir string, level int, noSandbox bool) *proto.ToolResponse {
	return c.runProcess(ctx, sid, msgID, action, append([]string{action}, argv...), workdir, level, noSandbox)
}

// runProcess 是子进程执行的共享实现（runLocal / runSSH）：display 为展示名
// （命令头），argv 为完整执行参数（argv[0] = 程序名）。
// granted level 与 nosandbox 随行传给 exec_procs（§5.10：沙箱去留只由
// 显式 nosandbox 决定——审批通过（9）不豁免沙箱）；
// workdir 空 = 继承 host 进程 cwd（请求级缺省回落在 execCmd 层完成）。
func (c *Client) runProcess(ctx context.Context, sid, msgID, display string, argv []string, workdir string, level int, noSandbox bool) *proto.ToolResponse {
	action := argv[0]
	logPath := filepath.Join(c.sessionWorkDir(sid), ".exec", msgID+".log")
	if err := c.ensureSessionWorkDir(sid); err != nil {
		return errResp(msgID, err.Error())
	}
	netDeny, netAllow := c.netPol.Snapshot(sid)
	opts := exec_procs.StartOptions{
		ID:           fmt.Sprintf("%s:%s:%s", c.hostID, sid, msgID),
		Command:      strings.TrimSpace(display + " " + strings.Join(argv[1:], " ")),
		LogPath:      logPath,
		Workdir:      workdir,
		Exec:         argv,
		Level:        level,
		NoSandbox:    noSandbox,
		WriteRoots:   c.policy.WriteRootsFor(sid), // fs 域：cfg fs_allow + 临时 grant
		DenyPaths:    c.policy.DenyPatterns(),     // fs 域 deny 名单（默认表 + cfg fs_deny）
		SandboxRules: fsSandboxRules(c.policy),    // fs 域有序规则表快照（M3 行序映射）
		WritePaths:   c.policy.WritePatternsFor(sid),
		FsOpen:       c.policy.OpenMode(), // fs_policy=open 快照
		NetOpen:      c.netPol.OpenMode(), // net_policy=open 快照
		NetDeny:      netDeny,             // net 域 deny/allow 快照（含内建 localhost:*）
		NetAllow:     netAllow,
	}
	if out := exec_procs.Output(ctx); out != nil {
		code, err := c.procs.RunProcess(ctx, opts, out)
		c.procs.RecordExit(ctx, code)
		if err != nil {
			return errResp(msgID, err.Error())
		}
		return &proto.ToolResponse{MsgID: msgID, State: proto.StateCompleted, Attrs: map[string]string{"exit_code": strconv.Itoa(code)}}
	}
	res, err := c.procs.Start(ctx, opts)
	if err != nil {
		return errResp(msgID, err.Error())
	}
	if res.Background {
		// 请求 deadline 到期 → 自动转后台
		return &proto.ToolResponse{
			MsgID:   msgID,
			State:   proto.StateCompleted,
			Content: res.Content,
			Attrs: map[string]string{
				"action":     action,
				"path":       res.LogPath,
				"rows":       strconv.Itoa(res.Lines),
				"truncated":  strconv.FormatBool(res.Truncated),
				"background": "true",
				"id":         res.ID,
			},
		}
	}
	return &proto.ToolResponse{
		MsgID:   msgID,
		State:   proto.StateCompleted,
		Content: res.Content,
		Attrs: map[string]string{
			"action":    action,
			"path":      res.LogPath,
			"rows":      strconv.Itoa(res.Lines),
			"truncated": strconv.FormatBool(res.Truncated),
			"exit_code": strconv.Itoa(res.ExitCode),
		},
	}
}

func errResp(msgID, msg string) *proto.ToolResponse {
	return &proto.ToolResponse{MsgID: msgID, State: proto.StateError, Error: msg}
}

// fsSandboxRules 把 fs 域有序规则表映射为沙箱行序快照（M3 行序映射输入）：
// 与工具层判定同源（builtin + cfg 拼接序，后规则胜）；darwin 按表序输出，
// 其余平台消费 deny/writeAllow 字段不受影响。
func fsSandboxRules(p *fsauth.Policy) []exec_procs.SandboxRule {
	rows := p.Rules()
	out := make([]exec_procs.SandboxRule, 0, len(rows))
	for _, r := range rows {
		out = append(out, exec_procs.SandboxRule{Effect: r.Effect, Patterns: r.Patterns})
	}
	return out
}
