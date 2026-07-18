package aicenv

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

const execMaxLines = 1000
const bgCmdMaxLen = 120

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
}

var (
	bgRegistry   = make(map[string]*bgProcess) // key: logPath
	bgRegistryMu sync.Mutex
)

// ExecTool 返回内置 exec 工具（命令执行）。
// action 即要执行的程序名（bash、ls、python 等），argv 为其参数。
// 输出重定向到 {tmp}/aic/{session_id}/{msg_id}.log，响应截断前 1K 行。
func ExecTool(workDir string, execTimeout time.Duration) Tool {
	return Tool{
		Def: ToolDef{
			Name:        "exec",
			Description: "Execute a program. action is the program name (bash, sh, ls, python, git, ...), argv are its arguments. Output is logged to file, response truncated to 1000 lines. Use fs read to get full log.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"action": map[string]any{"type": "string", "description": "Program name or shell to execute"},
					"argv":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				},
				"required": []string{"action", "argv"},
			},
			RequiredLevel: 2,
			PolicyVersion: "1",
		},
		Handler: func(ctx context.Context, data any) (*ToolResult, error) {
			params := ParseToolParams(data)
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

	// 内置命令
	if params.Action == "bg_list" {
		return handleBgList(sessionID)
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

	// 后台 context：不继承服务端 deadline，超时由 execTimeout 控制
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
	pid := cmd.Process.Pid

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
		f.Close()
		bgCancel()

		// 后台命令结束后从注册表移除
		bgRegistryMu.Lock()
		delete(bgRegistry, logPath)
		bgRegistryMu.Unlock()
	}()

	select {
	case runErr := <-done:
		// 命令在服务端 deadline 前完成
		return finishExec(logPath, runErr)

	case <-ctx.Done():
		// 服务端 deadline 到期，命令转入后台
		bgRegistryMu.Lock()
		bgRegistry[logPath] = &bgProcess{
			sessionID: sessionID,
			msgID:     msgID,
			pid:       pid,
			action:    params.Action,
			argv:      params.Argv,
			logPath:   logPath,
			startTime: time.Now(),
			timeout:   execTimeout,
			cancel:    bgCancel,
		}
		bgRegistryMu.Unlock()

		// 返回当前输出，命令继续后台运行
		content, lines, truncated := readHeadLines(logPath, execMaxLines)
		return &ToolResult{
			Content: content,
			Attrs: map[string]string{
				"path":       logPath,
				"rows":       strconv.Itoa(lines),
				"truncated":  strconv.FormatBool(truncated),
				"background": "true",
			},
		}, nil
	}
}

type bgItemJSON struct {
	PID       int    `json:"pid"`
	LogPath   string `json:"log_path"`
	Command   string `json:"command"`
	Elapsed   string `json:"elapsed"`
	Remaining string `json:"remaining"`
}

func handleBgList(sessionID string) (*ToolResult, error) {
	bgRegistryMu.Lock()
	defer bgRegistryMu.Unlock()

	now := time.Now()
	items := make([]bgItemJSON, 0)
	for _, p := range bgRegistry {
		if p.sessionID != sessionID {
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
	attrs := map[string]string{
		"action": "bg_list",
		"rows":   strconv.Itoa(len(items)),
	}

	return &ToolResult{Content: string(data), Attrs: attrs}, nil
}

func finishExec(logPath string, runErr error) (*ToolResult, error) {
	content, lines, truncated := readHeadLines(logPath, execMaxLines)

	attrs := map[string]string{
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
