package aicenv

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

const execMaxLines = 1000

// ExecTool 返回内置 exec 工具（命令执行）。
// action 即要执行的程序名（bash、ls、python 等），argv 为其参数。
// 输出重定向到 {tmp}/aic/{session_id}/{msg_id}.log，响应截断前 1K 行。
func ExecTool(workDir string) Tool {
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
			return handleExec(ctx, params, workDir, sessionID, msgID)
		},
	}
}

func handleExec(ctx context.Context, params *ToolParams, workDir, sessionID, msgID string) (*ToolResult, error) {
	if params.Action == "" {
		return &ToolResult{Error: "exec: action is required"}, nil
	}

	execCtx, cancel := context.WithTimeout(ctx, 50*time.Second)
	defer cancel()

	logDir := filepath.Join(os.TempDir(), "aic", sessionID)
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return &ToolResult{Error: fmt.Sprintf("exec: create log dir: %v", err)}, nil
	}
	logPath := filepath.Join(logDir, msgID+".log")

	f, err := os.Create(logPath)
	if err != nil {
		return &ToolResult{Error: fmt.Sprintf("exec: create log file: %v", err)}, nil
	}

	cmd := exec.CommandContext(execCtx, params.Action, params.Argv...)
	cmd.Dir = workDir
	setSysProcAttr(cmd)
	cmd.Stdout = f
	cmd.Stderr = f

	runErr := cmd.Run()
	f.Close()

	content, lines, truncated := readHeadLines(logPath, execMaxLines)

	attrs := map[string]string{
		"path":      logPath,
		"rows":      strconv.Itoa(lines),
		"truncated": strconv.FormatBool(truncated),
	}

	if runErr != nil {
		if execCtx.Err() == context.DeadlineExceeded {
			return &ToolResult{Content: content, Attrs: attrs, Error: "exec: command timed out after 50s"}, nil
		}
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
	var out []byte
	for lines < maxLines && scanner.Scan() {
		out = append(out, scanner.Bytes()...)
		out = append(out, '\n')
		lines++
	}
	if scanner.Scan() {
		truncated = true
		lines++
	}
	if err := scanner.Err(); err != nil {
		truncated = true
	}
	return string(out), lines, truncated
}
