package aichost

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
)

// exec 标准虚拟指令（§6.2）：经 caps exec.actions 声明的标准虚拟指令，
// 所有 host agent 的实现必须一致。
const (
	execMaxLines  = 1000 // 响应返回日志前 1000 行，完整日志用 fs read 获取
	bgCmdMaxLen   = 120  // bg_list 中 command 截断长度
	bgWaitDefault = 30   // bg_wait --wait 缺省秒数
)

// bgProcess 是后台进程注册表条目。
// 进程结束后条目保留（含退出码），供 bg_wait 取结果；host agent 重启后
// 注册表丢失——OS 进程可能仍在运行但已不可追踪（§6.3 重启语义）。
type bgProcess struct {
	sessionID string
	msgID     string
	pid       int
	action    string
	argv      []string
	logPath   string
	startTime time.Time
	timeout   time.Duration
	cancel    context.CancelFunc

	done     bool
	exitCode int
}

var (
	bgRegistry   = make(map[string]*bgProcess) // key: logPath
	bgRegistryMu sync.Mutex
)

// ExecTool 返回内置 exec 工具（§6：开放 action 空间，action 即程序名）。
// argv 原样作为程序参数（豁免通用 flag 解析，交错顺序保留）。
// 输出合并写入 {tmp}/aic/{session_id}/{msg_id}.log，响应截断前 1000 行。
func ExecTool(workDir string, execTimeout time.Duration) Tool {
	return Tool{
		Def: ToolDef{
			Name:        "exec",
			Description: "Execute a program. action is the program name (bash, sh, ls, python, git, ...) or a virtual action (bg_list/bg_wait/bg_kill manage background processes). argv are passed verbatim as program arguments. Output is logged to file, response truncated to 1000 lines (use fs read for the full log). Timed-out commands auto-background; track with bg_list, wait with bg_wait <log_path>, kill with bg_kill <log_path>.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"action": map[string]any{"type": "string", "description": "Program name or virtual action (bg_list, bg_wait, bg_kill)"},
					"argv":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				},
				"required": []string{"action", "argv"},
			},
			// §10.2：虚拟指令经 caps exec.actions 声明（字符串简写继承基线 3）；
			// 未列出的可执行文件按指令集基线 All(3) 检查
			Actions:       []any{"bg_list", "bg_wait", "bg_kill"},
			RequiredLevel: 3,
			PolicyVersion: "1",
		},
		Handler: func(ctx context.Context, data any) (*ToolResult, error) {
			params, err := ParseToolParams(data)
			if err != nil {
				return &ToolResult{Error: "exec: " + err.Error()}, nil
			}
			rc := RequestFromContext(ctx)
			var sessionID, msgID string
			if rc != nil {
				sessionID = rc.SessionID
				msgID = rc.MsgID
			}
			return handleExec(ctx, params, workDir, sessionID, msgID, execTimeout)
		},
	}
}

func handleExec(ctx context.Context, params *ToolParams, workDir, sessionID, msgID string, execTimeout time.Duration) (*ToolResult, error) {
	if params.Action == "" {
		return &ToolResult{Error: "exec: action is required"}, nil
	}

	// §6.1 host 端 action 解析顺序（写死）：
	// 先匹配 caps 声明的自定义虚拟指令 → 再按 PATH/系统可执行文件查找 → unknown action
	switch params.Action {
	case "bg_list":
		return handleBgList(sessionID)
	case "bg_wait":
		return handleBgWait(params)
	case "bg_kill":
		return handleBgKill(params)
	}
	if _, err := exec.LookPath(params.Action); err != nil {
		return &ToolResult{Error: fmt.Sprintf("exec: unknown action %q", params.Action)}, nil
	}

	logDir := filepath.Join(os.TempDir(), "aic", sessionID)
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return &ToolResult{Error: fmt.Sprintf("exec: create log dir: %v", err)}, nil
	}
	logPath := filepath.Join(logDir, msgID+".log")

	f, err := os.Create(logPath)
	if err != nil {
		return &ToolResult{Error: fmt.Sprintf("exec: create log file: %v", err)}, nil
	}

	// 后台 context：不继承服务端 deadline，host 端自有超时（默认 10m，§6.3）
	bgCtx, bgCancel := context.WithTimeout(context.Background(), execTimeout)

	cmd := exec.CommandContext(bgCtx, params.Action, params.Argv...)
	cmd.Dir = workDir
	setSysProcAttr(cmd)
	cmd.Stdout = f
	cmd.Stderr = f

	if err := cmd.Start(); err != nil {
		f.Close()
		bgCancel()
		return &ToolResult{Error: fmt.Sprintf("exec: %v", err)}, nil
	}

	bg := &bgProcess{
		sessionID: sessionID,
		msgID:     msgID,
		pid:       cmd.Process.Pid,
		action:    params.Action,
		argv:      params.Argv,
		logPath:   logPath,
		startTime: time.Now(),
		timeout:   execTimeout,
		cancel:    bgCancel,
	}
	bgRegistryMu.Lock()
	bgRegistry[logPath] = bg
	bgRegistryMu.Unlock()

	done := make(chan error, 1)
	go func() {
		runErr := cmd.Wait()
		f.Close()
		bgCancel()
		bgRegistryMu.Lock()
		bg.done = true
		bg.exitCode = 0
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			bg.exitCode = exitErr.ExitCode()
		} else if runErr != nil {
			bg.exitCode = -1
		}
		bgRegistryMu.Unlock()
		done <- runErr
	}()

	select {
	case runErr := <-done:
		// 命令在请求 deadline 前完成
		return finishExec(params.Action, logPath, runErr)

	case <-ctx.Done():
		// 请求 deadline 到期，命令转入后台（§6.3）：
		// 返回当前已产出的前 1000 行 + background=true，用 bg_list 跟踪
		content, lines, truncated := readHeadLines(logPath, execMaxLines)
		return &ToolResult{
			Content: content,
			Attrs: map[string]string{
				"action":     params.Action,
				"path":       logPath,
				"rows":       strconv.Itoa(lines),
				"truncated":  strconv.FormatBool(truncated),
				"background": "true",
			},
		}, nil
	}
}

// ---- bg_list ----

type bgItemJSON struct {
	PID       int    `json:"pid"`
	LogPath   string `json:"log_path"`
	Command   string `json:"command"`
	Elapsed   string `json:"elapsed"`
	Remaining string `json:"remaining"`
}

// handleBgList 列出本 session 仍在运行的后台进程（§6.2：Content 为 JSON 数组）。
func handleBgList(sessionID string) (*ToolResult, error) {
	bgRegistryMu.Lock()
	defer bgRegistryMu.Unlock()

	now := time.Now()
	items := make([]bgItemJSON, 0)
	for _, p := range bgRegistry {
		if p.sessionID != sessionID || p.done {
			continue
		}
		elapsed := now.Sub(p.startTime).Truncate(time.Second)
		remaining := max(p.timeout-now.Sub(p.startTime), 0).Truncate(time.Second)
		cmdStr := p.action
		if len(p.argv) > 0 {
			s := strings.Join(p.argv, " ")
			if len(s) > bgCmdMaxLen {
				s = s[:bgCmdMaxLen] + "..."
			}
			cmdStr += " " + s
		}
		items = append(items, bgItemJSON{
			PID:       p.pid,
			LogPath:   p.logPath,
			Command:   cmdStr,
			Elapsed:   elapsed.String(),
			Remaining: remaining.String(),
		})
	}

	data, _ := json.Marshal(items)
	return &ToolResult{
		Content: string(data),
		Attrs: map[string]string{
			"action": "bg_list",
			"rows":   strconv.Itoa(len(items)),
		},
	}, nil
}

// ---- bg_wait ----

// handleBgWait 等待后台进程结束，补全"后台化→取结果"闭环（§6.2）。
//
// argv: <log_path> [--wait N]
//   - 进程在 --wait 秒（缺省 30）内结束 → 日志前 1000 行 + exit_code/path/rows/truncated
//   - --wait 到期仍在运行 → 当前输出前 1000 行 + background=true（可再次 bg_wait）
//   - log_path 不存在/进程已终结且不在注册表 → 统一错误（此时可用 fs read 读日志）
func handleBgWait(params *ToolParams) (*ToolResult, error) {
	if len(params.Argv) == 0 {
		return &ToolResult{Error: "exec bg_wait: log_path is required"}, nil
	}
	logPath := params.Argv[0]
	waitSecs := bgWaitDefault
	for i := 1; i+1 < len(params.Argv); i++ {
		if params.Argv[i] == "--wait" {
			if n, err := strconv.Atoi(params.Argv[i+1]); err == nil && n > 0 {
				waitSecs = n
			}
			i++
		}
	}

	bgRegistryMu.Lock()
	p, ok := bgRegistry[logPath]
	bgRegistryMu.Unlock()
	if !ok {
		return &ToolResult{Error: fmt.Sprintf("exec bg_wait: no such background process: %s", logPath)}, nil
	}

	deadline := time.Now().Add(time.Duration(waitSecs) * time.Second)
	for {
		bgRegistryMu.Lock()
		done := p.done
		exitCode := p.exitCode
		bgRegistryMu.Unlock()
		if done {
			content, lines, truncated := readHeadLines(logPath, execMaxLines)
			return &ToolResult{
				Content: content,
				Attrs: map[string]string{
					"action":    "bg_wait",
					"path":      logPath,
					"rows":      strconv.Itoa(lines),
					"truncated": strconv.FormatBool(truncated),
					"exit_code": strconv.Itoa(exitCode),
				},
			}, nil
		}
		if time.Now().After(deadline) {
			content, lines, truncated := readHeadLines(logPath, execMaxLines)
			return &ToolResult{
				Content: content,
				Attrs: map[string]string{
					"action":     "bg_wait",
					"path":       logPath,
					"rows":       strconv.Itoa(lines),
					"truncated":  strconv.FormatBool(truncated),
					"background": "true",
				},
			}, nil
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// ---- bg_kill ----

// handleBgKill 终止本 session 的后台进程（§6.2）。
// Unix 先发 SIGTERM，5s 未退出补 SIGKILL；Windows 用 TerminateProcess。
// 权限随 exec 基线 All(3)——能启动即应能终止，不单独设槛。
func handleBgKill(params *ToolParams) (*ToolResult, error) {
	if len(params.Argv) == 0 {
		return &ToolResult{Error: "exec bg_kill: log_path is required"}, nil
	}
	logPath := params.Argv[0]

	bgRegistryMu.Lock()
	p, ok := bgRegistry[logPath]
	bgRegistryMu.Unlock()
	if !ok {
		return &ToolResult{Error: fmt.Sprintf("exec bg_kill: no such background process: %s", logPath)}, nil
	}

	bgRegistryMu.Lock()
	done := p.done
	exitCode := p.exitCode
	bgRegistryMu.Unlock()
	if done {
		// 已终结但仍在注册表：清理条目并报退出码（非错误）
		bgRegistryMu.Lock()
		delete(bgRegistry, logPath)
		bgRegistryMu.Unlock()
		return &ToolResult{
			Content: fmt.Sprintf("background process already exited: %s (pid %d, exit=%d)", logPath, p.pid, exitCode),
			Attrs: map[string]string{
				"action":    "bg_kill",
				"path":      logPath,
				"pid":       strconv.Itoa(p.pid),
				"exit_code": strconv.Itoa(exitCode),
			},
		}, nil
	}

	killBackground(p)

	bgRegistryMu.Lock()
	delete(bgRegistry, logPath)
	bgRegistryMu.Unlock()
	return &ToolResult{
		Content: fmt.Sprintf("killed background process: %s (pid %d)", logPath, p.pid),
		Attrs: map[string]string{
			"action": "bg_kill",
			"path":   logPath,
			"pid":    strconv.Itoa(p.pid),
		},
	}, nil
}

// ---- 内部 ----

func finishExec(action, logPath string, runErr error) (*ToolResult, error) {
	content, lines, truncated := readHeadLines(logPath, execMaxLines)

	attrs := map[string]string{
		"action":    action,
		"path":      logPath,
		"rows":      strconv.Itoa(lines),
		"truncated": strconv.FormatBool(truncated),
	}

	if runErr != nil {
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			return &ToolResult{Content: content, Attrs: attrs, Error: fmt.Sprintf("exec: %v (exit=%d)", runErr, exitErr.ExitCode())}, nil
		}
		return &ToolResult{Content: content, Attrs: attrs, Error: fmt.Sprintf("exec: %v", runErr)}, nil
	}
	return &ToolResult{Content: content, Attrs: attrs}, nil
}

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
		fmt.Fprintf(&b, "%d\t%s\n", lines, scanner.Text())
	}
	if scanner.Scan() {
		truncated = true
	}
	if err := scanner.Err(); err != nil {
		truncated = true
	}
	return strings.TrimRight(b.String(), "\n"), lines, truncated
}
