package host

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/veypi/aic-pod/sdk/proto"
	"github.com/veypi/aic-pod/sdk/vcore"
)

// 程序命令执行（§5.9）：stdout+stderr 合并写入 {tmp}/aic/{session_id}/{msg_id}.log；
// 请求 deadline 内完成 → 返回日志前 1000 行（不包装行号，§2.2）；
// 到期未完成 → 自动转后台（host 端自有超时，默认 10m），
// 返回当前前 1000 行 + background=true + bg id（{host}:{sid}:{op_id}）。

const (
	execMaxLines  = 1000
	bgCmdMaxLen   = 120
	bgWaitDefault = 30
)

// bgProcess 是后台进程注册表条目。
// 进程结束后条目保留（含退出码），供 bg_wait 取结果；host agent 重启后
// 注册表丢失——OS 进程可能仍在运行但已不可追踪（§5.8 重启语义）。
type bgProcess struct {
	id        string // {host}:{sid}:{op_id}
	sessionID string
	pid       int
	command   string
	logPath   string
	startTime time.Time
	timeout   time.Duration
	cancel    context.CancelFunc

	done     chan struct{}
	exitCode int
}

var doneToken = struct{}{}

type bgRegistry struct {
	mu    sync.Mutex
	procs map[string]*bgProcess
}

func newBgRegistry() *bgRegistry {
	return &bgRegistry{procs: map[string]*bgProcess{}}
}

// runProgram 执行程序命令（§5.9）。
func (c *Client) runProgram(ctx context.Context, sid, msgID, action string, argv []string, workdir string) *proto.ToolResponse {
	if _, err := exec.LookPath(action); err != nil {
		return errResp(msgID, fmt.Sprintf("exec: unknown action %q", action))
	}

	logDir := filepath.Join(os.TempDir(), "aic", sid)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return errResp(msgID, fmt.Sprintf("exec: create log dir: %v", err))
	}
	logPath := filepath.Join(logDir, msgID+".log")
	f, err := os.Create(logPath)
	if err != nil {
		return errResp(msgID, fmt.Sprintf("exec: create log file: %v", err))
	}

	// 后台 context：不继承请求 deadline，host 端自有超时（默认 10m）
	bgCtx, bgCancel := context.WithTimeout(context.Background(), c.opts.ExecTimeout)

	// workdir 作为进程 cwd（缺省 = host 端配置工作区，§2.1.1 透传层）；
	// argv 原样透传、相对路径由目标进程自行基于 cwd 解析
	dir := workdir
	if dir == "" {
		dir = c.opts.WorkDir
	}
	cmd := exec.CommandContext(bgCtx, action, argv...)
	cmd.Dir = dir
	setSysProcAttr(cmd)
	cmd.Stdout = f
	cmd.Stderr = f

	if err := cmd.Start(); err != nil {
		f.Close()
		bgCancel()
		return errResp(msgID, fmt.Sprintf("exec: %v", err))
	}

	bg := &bgProcess{
		id:        fmt.Sprintf("%s:%s:%s", c.hostID, sid, msgID),
		sessionID: sid,
		pid:       cmd.Process.Pid,
		command:   strings.TrimSpace(action + " " + strings.Join(argv, " ")),
		logPath:   logPath,
		startTime: time.Now(),
		timeout:   c.opts.ExecTimeout,
		cancel:    bgCancel,
		done:      make(chan struct{}),
	}
	c.bgs.mu.Lock()
	c.bgs.procs[bg.id] = bg
	c.bgs.mu.Unlock()

	go func() {
		runErr := cmd.Wait()
		f.Close()
		bgCancel()
		bg.exitCode = 0
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			bg.exitCode = exitErr.ExitCode()
		} else if runErr != nil {
			bg.exitCode = -1
		}
		bg.done <- doneToken
		close(bg.done)
	}()

	select {
	case <-bg.done:
		return finishExec(msgID, action, logPath, bg.exitCode)
	case <-ctx.Done():
		// 请求 deadline 到期 → 自动转后台：返回当前前 1000 行 + background=true + bg id
		content, lines, truncated := readHeadLines(logPath, execMaxLines)
		return &proto.ToolResponse{
			MsgID:   msgID,
			State:   proto.StateCompleted,
			Content: content,
			Attrs: map[string]string{
				"action":     action,
				"path":       logPath,
				"rows":       strconv.Itoa(lines),
				"truncated":  strconv.FormatBool(truncated),
				"background": "true",
				"id":         bg.id,
			},
		}
	}
}

// finishExec 完成程序命令：返回日志前 1000 行 + 非零退出标记（§5.9）。
func finishExec(msgID, action, logPath string, exitCode int) *proto.ToolResponse {
	content, lines, truncated := readHeadLines(logPath, execMaxLines)
	resp := &proto.ToolResponse{
		MsgID:   msgID,
		Content: content,
		Attrs: map[string]string{
			"action":    action,
			"path":      logPath,
			"rows":      strconv.Itoa(lines),
			"truncated": strconv.FormatBool(truncated),
			"exit_code": strconv.Itoa(exitCode),
		},
	}
	if exitCode != 0 {
		resp.State = proto.StateError
		resp.Error = fmt.Sprintf("exec: exit status %d (exit=%d)", exitCode, exitCode)
	} else {
		resp.State = proto.StateCompleted
	}
	return resp
}

// ---- bg_list / bg_wait / bg_kill（§5.8） ----

func (r *bgRegistry) list(sid string) *vcore.Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	type item struct {
		ID        string `json:"id"`
		PID       int    `json:"pid"`
		Command   string `json:"command"`
		Elapsed   string `json:"elapsed"`
		Remaining string `json:"remaining"`
	}
	items := []item{}
	now := time.Now()
	for _, p := range r.procs {
		if p.sessionID != sid {
			continue
		}
		select {
		case <-p.done:
			continue
		default:
		}
		cmdStr := p.command
		if len(cmdStr) > bgCmdMaxLen {
			cmdStr = cmdStr[:bgCmdMaxLen]
		}
		elapsed := now.Sub(p.startTime).Truncate(time.Second)
		remaining := max(p.timeout-now.Sub(p.startTime), 0).Truncate(time.Second)
		items = append(items, item{p.id, p.pid, cmdStr, elapsed.String(), remaining.String()})
	}
	data, _ := json.Marshal(items)
	return &vcore.Result{
		Content: string(data),
		Attrs:   map[string]string{"action": "bg_list", "rows": strconv.Itoa(len(items))},
	}
}

func (r *bgRegistry) wait(ctx context.Context, sid string, argv []string) (*vcore.Result, error) {
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
	r.mu.Lock()
	p, ok := r.procs[id]
	r.mu.Unlock()
	if !ok || p.sessionID != sid {
		return nil, fmt.Errorf("exec bg_wait: no such background process: %s", id)
	}

	select {
	case <-p.done:
		content, lines, truncated := readHeadLines(p.logPath, execMaxLines)
		return &vcore.Result{
			Content: content,
			Attrs: map[string]string{
				"action":    "bg_wait",
				"path":      p.logPath,
				"rows":      strconv.Itoa(lines),
				"truncated": strconv.FormatBool(truncated),
				"exit_code": strconv.Itoa(p.exitCode),
			},
		}, nil
	case <-time.After(time.Duration(waitSecs) * time.Second):
		content, lines, truncated := readHeadLines(p.logPath, execMaxLines)
		return &vcore.Result{
			Content: content,
			Attrs: map[string]string{
				"action":     "bg_wait",
				"path":       p.logPath,
				"rows":       strconv.Itoa(lines),
				"truncated":  strconv.FormatBool(truncated),
				"background": "true",
			},
		}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r *bgRegistry) kill(sid string, argv []string) (*vcore.Result, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("exec bg_kill: id is required")
	}
	id := argv[0]
	r.mu.Lock()
	p, ok := r.procs[id]
	r.mu.Unlock()
	if !ok || p.sessionID != sid {
		return nil, fmt.Errorf("exec bg_kill: no such background process: %s", id)
	}

	select {
	case <-p.done:
		r.mu.Lock()
		delete(r.procs, id)
		r.mu.Unlock()
		return nil, fmt.Errorf("exec bg_kill: no such background process: %s", id)
	default:
	}

	killBackground(p)
	r.mu.Lock()
	delete(r.procs, id)
	r.mu.Unlock()
	return &vcore.Result{
		Content: fmt.Sprintf("killed background process: %s (pid %d)", id, p.pid),
		Attrs:   map[string]string{"action": "bg_kill", "pid": strconv.Itoa(p.pid)},
	}, nil
}

// readHeadLines 读取日志前 maxLines 行（不包装行号，§2.2/§5.9）。
func readHeadLines(path string, maxLines int) (content string, lines int, truncated bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	var b strings.Builder
	for lines < maxLines && scanner.Scan() {
		lines++
		b.WriteString(scanner.Text())
		b.WriteByte('\n')
	}
	if scanner.Scan() {
		truncated = true
	}
	if err := scanner.Err(); err != nil {
		truncated = true
	}
	return strings.TrimRight(b.String(), "\n"), lines, truncated
}

func errResp(msgID, msg string) *proto.ToolResponse {
	return &proto.ToolResponse{MsgID: msgID, State: proto.StateError, Error: msg}
}
