package host

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/veypi/aic-pod/sdk/exec_procs"
	"github.com/veypi/aic-pod/sdk/proto"
	"github.com/veypi/aic-pod/sdk/vcore"
)

// 程序命令执行（§5.9）：stdout+stderr 合并写入 {tmp}/aic/{session_id}/{msg_id}.log；
// 请求 deadline 内完成 → 返回日志前 1000 行（不包装行号，§2.2）；
// 到期未完成 → 自动转后台（host 端自有超时，默认 10m），
// 返回当前前 1000 行 + background=true + bg id（{host}:{sid}:{op_id}）。

const (
	bgCmdMaxLen   = 120
	bgWaitDefault = 30
)

// runProgram 执行程序命令（§5.9）：子进程经 exec_procs 统一托管——
// stdout+stderr 合并写入 {tmp}/aic/{session_id}/{msg_id}.log；
// 请求 deadline 内完成 → 返回日志前 1000 行（不包装行号，§2.2）；
// 到期未完成 → 自动转后台（host 端自有超时，默认 10m），
// 返回当前前 1000 行 + background=true + bg id（{host}:{sid}:{op_id}）。
func (c *Client) runProgram(ctx context.Context, sid, msgID, action string, argv []string, workdir string) *proto.ToolResponse {
	logPath := filepath.Join(os.TempDir(), "aic", sid, msgID+".log")
	res, err := c.procs.Start(ctx, exec_procs.StartOptions{
		ID:      fmt.Sprintf("%s:%s:%s", c.hostID, sid, msgID),
		Command: strings.TrimSpace(action + " " + strings.Join(argv, " ")),
		LogPath: logPath,
		Workdir: workdir, // 缺省 = host 端配置工作区（调用方已填充）
		Exec:    append([]string{action}, argv...),
	})
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

// bgList 实现 bg_list（§5.8）：经 exec_procs 统一管理。
func (c *Client) bgList(sid string) *vcore.Result {
	type item struct {
		ID        string `json:"id"`
		PID       int    `json:"pid"`
		Command   string `json:"command"`
		Elapsed   string `json:"elapsed"`
		Remaining string `json:"remaining"`
	}
	items := []item{}
	now := time.Now()
	for _, e := range c.procs.List() {
		if !strings.HasPrefix(e.ID, c.hostID+":"+sid+":") {
			continue
		}
		cmdStr := e.Command
		if len(cmdStr) > bgCmdMaxLen {
			cmdStr = cmdStr[:bgCmdMaxLen]
		}
		elapsed := now.Sub(e.Started).Truncate(time.Second)
		remaining := max(e.Timeout-now.Sub(e.Started), 0).Truncate(time.Second)
		items = append(items, item{e.ID, e.PID(), cmdStr, elapsed.String(), remaining.String()})
	}
	data, _ := json.Marshal(items)
	return &vcore.Result{
		Content: string(data),
		Attrs:   map[string]string{"action": "bg_list", "rows": strconv.Itoa(len(items))},
	}
}

// bgWait 实现 bg_wait（§5.8）。
func (c *Client) bgWait(ctx context.Context, sid string, argv []string) (*vcore.Result, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("exec bg_wait: id is required")
	}
	id := argv[0]
	waitSecs := bgWaitDefault
	for i := 1; i+1 < len(argv); i++ {
		if argv[i] == "--wait" {
			if n, err := strconv.Atoi(argv[i+1]); err == nil && n > 0 {
				waitSecs = n
			}
			i++
		}
	}
	res, err := c.procs.Wait(ctx, id, time.Duration(waitSecs)*time.Second)
	if err != nil {
		return nil, fmt.Errorf("exec bg_wait: %s", err)
	}
	attrs := map[string]string{"action": "bg_wait", "path": res.LogPath, "rows": strconv.Itoa(res.Lines)}
	if res.Truncated {
		attrs["truncated"] = "true"
	}
	if res.Background {
		attrs["background"] = "true"
	} else {
		attrs["exit_code"] = strconv.Itoa(res.ExitCode)
	}
	return &vcore.Result{Content: res.Content, Attrs: attrs}, nil
}

// bgKill 实现 bg_kill（§5.8）。
func (c *Client) bgKill(sid string, argv []string) (*vcore.Result, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("exec bg_kill: id is required")
	}
	id := argv[0]
	// 越权防护：id 必须属于本 session
	if !strings.HasPrefix(id, c.hostID+":"+sid+":") {
		return nil, fmt.Errorf("exec bg_kill: no such background process: %s", id)
	}
	e := c.procs.Get(id)
	if e == nil {
		return nil, fmt.Errorf("exec bg_kill: no such background process: %s", id)
	}
	if err := c.procs.Kill(id); err != nil {
		return nil, fmt.Errorf("exec bg_kill: %s", err)
	}
	return &vcore.Result{
		Content: fmt.Sprintf("killed background process: %s (pid %d)", id, e.PID()),
		Attrs:   map[string]string{"action": "bg_kill", "pid": strconv.Itoa(e.PID())},
	}, nil
}

func errResp(msgID, msg string) *proto.ToolResponse {
	return &proto.ToolResponse{MsgID: msgID, State: proto.StateError, Error: msg}
}
