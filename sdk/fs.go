package aicenv

import (
	"context"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ParseToolParams 从工具请求的 toolData 解析标准 action + argv 参数。
func ParseToolParams(toolData any) *ToolParams {
	if m, ok := toolData.(map[string]any); ok {
		action, _ := m["action"].(string)
		argv := toStringSlice(m["argv"])
		return &ToolParams{Action: strings.TrimSpace(action), Argv: argv}
	}
	return &ToolParams{}
}

// FsTool 返回内置 fs 工具（文件操作）。
func FsTool() Tool {
	return Tool{
		Def: ToolDef{
			Name:        "fs",
			Description: "File system operations: ls, read, write, edit, rm, mkdir, cp, mv, search.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"action": map[string]any{
						"type": "string",
						"enum": []string{"ls", "read", "write", "edit", "rm", "mkdir", "cp", "mv", "search"},
					},
					"argv": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				},
				"required": []string{"action", "argv"},
			},
			RequiredLevel: 1,
			PolicyVersion: "1",
		},
		Handler: func(ctx context.Context, data any) (*ToolResult, error) {
			params := ParseToolParams(data)
			return handleFs(ctx, params)
		},
	}
}

func handleFs(_ context.Context, params *ToolParams) (*ToolResult, error) {
	switch params.Action {
	case "read":
		return handleFsRead(params)
	case "write":
		return handleFsWrite(params)
	case "edit":
		return handleFsEdit(params)
	case "ls":
		return handleFsLs(params)
	case "rm":
		return handleFsRemove(params)
	case "mkdir":
		return handleFsMkdir(params)
	case "cp":
		return handleFsCopy(params)
	case "mv":
		return handleFsMove(params)
	case "search":
		return handleFsSearch(params)
	default:
		return &ToolResult{
			Error: fmt.Sprintf("fs: unknown action %q (supported: ls, read, write, edit, rm, mkdir, cp, mv, search)", params.Action),
		}, nil
	}
}

// ---- fs handlers ----

func handleFsRead(params *ToolParams) (*ToolResult, error) {
	offset := 0
	limit := -1
	argv := params.Argv

	offsetStr, argv := extractFlag(argv, "--offset")
	limitStr, argv := extractFlag(argv, "--limit")
	if v, err := fmt.Sscanf(offsetStr, "%d", &offset); err != nil || v != 1 {
		offset = 0
	}
	if v, err := fmt.Sscanf(limitStr, "%d", &limit); err != nil || v != 1 || limit <= 0 || limit > 1000 {
		limit = 1000
	}
	if len(argv) == 0 {
		return &ToolResult{Error: "fs read: path is required"}, nil
	}

	path := argv[0]
	data, err := os.ReadFile(path)
	if err != nil {
		return &ToolResult{Error: err.Error()}, nil
	}

	mimeType := detectMime(data)
	attrs := map[string]string{
		"action": "read",
		"path":   path,
		"mime":   mimeType,
	}

	if !isTextMime(mimeType) {
		attrs["size"] = strconv.Itoa(len(data))
		return &ToolResult{
			Content: fmt.Sprintf("Binary file: %s (%s, %d bytes)", path, mimeType, len(data)),
			Attrs:   attrs,
		}, nil
	}

	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	totalLines := len(lines)
	if totalLines > 0 && lines[totalLines-1] == "" {
		lines = lines[:totalLines-1]
		totalLines--
	}

	attrs["rows"] = strconv.Itoa(totalLines)

	if offset == 0 && limit == 1000 && totalLines <= 1000 {
		var b strings.Builder
		for i, line := range lines {
			fmt.Fprintf(&b, "%d\t%s\n", i+1, line)
		}
		content := strings.TrimRight(b.String(), "\n")
		attrs["range"] = fmt.Sprintf("1-%d", totalLines)
		attrs["truncated"] = "false"
		return &ToolResult{Content: content, Attrs: attrs}, nil
	}

	if offset < 0 {
		offset = 0
	}
	start := offset
	if start >= totalLines {
		return &ToolResult{Error: fmt.Sprintf("fs read: offset %d exceeds %d lines", offset, totalLines)}, nil
	}
	end := start + limit
	truncated := false
	if end > totalLines {
		end = totalLines
	} else {
		truncated = end < totalLines
	}

	selected := lines[start:end]
	var b strings.Builder
	for i, line := range selected {
		fmt.Fprintf(&b, "%d\t%s\n", start+i+1, line)
	}
	content := strings.TrimRight(b.String(), "\n")
	attrs["range"] = fmt.Sprintf("%d-%d", start+1, end)
	attrs["truncated"] = strconv.FormatBool(truncated)
	return &ToolResult{Content: content, Attrs: attrs}, nil
}

func handleFsWrite(params *ToolParams) (*ToolResult, error) {
	argv := params.Argv
	contentStr, argv := extractFlag(argv, "--content")
	if len(argv) == 0 {
		return &ToolResult{Error: "fs write: path is required"}, nil
	}
	path := argv[0]
	if err := os.WriteFile(path, []byte(contentStr), 0644); err != nil {
		return &ToolResult{Error: err.Error()}, nil
	}
	return &ToolResult{
		Content: fmt.Sprintf("wrote file: %s", path),
		Attrs:   map[string]string{"action": "write", "path": path},
	}, nil
}

func handleFsEdit(params *ToolParams) (*ToolResult, error) {
	argv := params.Argv
	oldStr, argv := extractFlag(argv, "--old")
	newStr, argv := extractFlag(argv, "--new")
	replaceAll := hasFlag(argv, "--replace-all")
	if len(argv) == 0 {
		return &ToolResult{Error: "fs edit: path is required"}, nil
	}
	path := argv[0]
	if oldStr == "" {
		return &ToolResult{Error: "fs edit: --old is required"}, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return &ToolResult{Error: err.Error()}, nil
	}
	content := string(data)
	if !strings.Contains(content, oldStr) {
		return &ToolResult{Error: "fs edit: --old string not found in file"}, nil
	}
	if replaceAll {
		content = strings.ReplaceAll(content, oldStr, newStr)
	} else {
		content = strings.Replace(content, oldStr, newStr, 1)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return &ToolResult{Error: err.Error()}, nil
	}
	return &ToolResult{
		Content: fmt.Sprintf("edited %s", path),
		Attrs:   map[string]string{"action": "edit", "path": path},
	}, nil
}

func handleFsLs(params *ToolParams) (*ToolResult, error) {
	path := "/"
	if len(params.Argv) > 0 {
		path = params.Argv[0]
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return &ToolResult{Error: err.Error()}, nil
	}

	var b strings.Builder
	for i, e := range entries {
		label := e.Name()
		if e.IsDir() {
			label += "/"
		}
		fmt.Fprintf(&b, "%d\t%s\n", i+1, label)
	}

	kind := "file"
	for _, e := range entries {
		if e.IsDir() {
			if kind == "file" {
				kind = "mixed"
			} else {
				kind = "directory"
			}
		}
	}
	if len(entries) == 0 {
		kind = "directory"
	}

	return &ToolResult{
		Content: strings.TrimRight(b.String(), "\n"),
		Attrs: map[string]string{
			"action":    "ls",
			"path":      path,
			"mime":      "text/plain",
			"rows":      strconv.Itoa(len(entries)),
			"path_kind": kind,
			"truncated": "false",
		},
	}, nil
}

func handleFsRemove(params *ToolParams) (*ToolResult, error) {
	if len(params.Argv) == 0 {
		return &ToolResult{Error: "fs rm: path is required"}, nil
	}
	path := params.Argv[0]
	if err := os.RemoveAll(path); err != nil {
		return &ToolResult{Error: err.Error()}, nil
	}
	return &ToolResult{
		Content: fmt.Sprintf("removed %s", path),
		Attrs:   map[string]string{"action": "rm", "path": path},
	}, nil
}

func handleFsMkdir(params *ToolParams) (*ToolResult, error) {
	if len(params.Argv) == 0 {
		return &ToolResult{Error: "fs mkdir: path is required"}, nil
	}
	path := params.Argv[0]
	if err := os.MkdirAll(path, 0755); err != nil {
		return &ToolResult{Error: err.Error()}, nil
	}
	return &ToolResult{
		Content: fmt.Sprintf("created %s", path),
		Attrs:   map[string]string{"action": "mkdir", "path": path},
	}, nil
}

func handleFsCopy(params *ToolParams) (*ToolResult, error) {
	if len(params.Argv) < 2 {
		return &ToolResult{Error: "fs cp: source and destination paths required"}, nil
	}
	src, dst := params.Argv[0], params.Argv[1]
	data, err := os.ReadFile(src)
	if err != nil {
		return &ToolResult{Error: err.Error()}, nil
	}
	if err := os.WriteFile(dst, data, 0644); err != nil {
		return &ToolResult{Error: err.Error()}, nil
	}
	return &ToolResult{
		Content: fmt.Sprintf("copied %s to %s", src, dst),
		Attrs:   map[string]string{"action": "cp", "path": dst},
	}, nil
}

func handleFsMove(params *ToolParams) (*ToolResult, error) {
	if len(params.Argv) < 2 {
		return &ToolResult{Error: "fs mv: source and destination paths required"}, nil
	}
	src, dst := params.Argv[0], params.Argv[1]
	if err := os.Rename(src, dst); err != nil {
		return &ToolResult{Error: err.Error()}, nil
	}
	return &ToolResult{
		Content: fmt.Sprintf("moved %s to %s", src, dst),
		Attrs:   map[string]string{"action": "mv", "path": dst},
	}, nil
}

func handleFsSearch(params *ToolParams) (*ToolResult, error) {
	argv := params.Argv
	glob, argv := extractFlag(argv, "--glob")
	pattern, argv := extractFlag(argv, "--pattern")
	limit := 100
	lv, argv := extractFlag(argv, "--limit")
	if n, err := fmt.Sscanf(lv, "%d", &limit); err != nil || n != 1 || limit <= 0 || limit > 500 {
		limit = 100
	}
	ignoreCase := hasFlag(argv, "--ignore-case")

	root := "."
	if len(argv) > 0 {
		root = argv[0]
	}

	if _, err := os.Stat(root); err != nil {
		return &ToolResult{Error: fmt.Sprintf("search: %s: %v", root, err)}, nil
	}

	var b strings.Builder
	count := 0
	walkFS(root, glob, pattern, ignoreCase, limit, &count, &b)

	if count == 0 {
		return &ToolResult{
			Content: fmt.Sprintf("no lines matched pattern %q in %s", pattern, root),
			Attrs: map[string]string{
				"action":    "search",
				"path":      root,
				"mime":      "text/plain",
				"rows":      "0",
				"truncated": "false",
			},
		}, nil
	}

	return &ToolResult{
		Content: strings.TrimRight(b.String(), "\n"),
		Attrs: map[string]string{
			"action":    "search",
			"path":      root,
			"rows":      strconv.Itoa(count),
			"truncated": strconv.FormatBool(count >= limit),
		},
	}, nil
}

// ---- argv helpers ----

func extractFlag(argv []string, flag string) (value string, rest []string) {
	for i := 0; i < len(argv); i++ {
		if argv[i] == flag && i+1 < len(argv) {
			rest = make([]string, 0, len(argv)-2)
			rest = append(rest, argv[:i]...)
			rest = append(rest, argv[i+2:]...)
			return argv[i+1], rest
		}
	}
	return "", argv
}

func hasFlag(argv []string, flag string) bool {
	for _, a := range argv {
		if a == flag {
			return true
		}
	}
	return false
}

func toStringSlice(v any) []string {
	switch arr := v.(type) {
	case []any:
		out := make([]string, len(arr))
		for i, item := range arr {
			out[i] = fmt.Sprint(item)
		}
		return out
	case []string:
		return arr
	}
	return nil
}

// ---- glob + search helpers ----

func walkFS(root, glob, pattern string, ignoreCase bool, limit int, count *int, b *strings.Builder) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		if *count >= limit {
			return
		}
		subPath := filepath.Join(root, e.Name())
		if glob != "" {
			matched := matchSimpleGlob(glob, e.Name())
			if !matched {
				if e.IsDir() {
					walkFS(subPath, glob, pattern, ignoreCase, limit, count, b)
				}
				continue
			}
		}
		if pattern != "" && !e.IsDir() {
			data, err := os.ReadFile(subPath)
			if err != nil {
				continue
			}
			for ln, line := range strings.Split(string(data), "\n") {
				if *count >= limit {
					return
				}
				if matchLine(line, pattern, ignoreCase) {
					col := matchColumn(line, pattern, ignoreCase)
					*count++
					fmt.Fprintf(b, "%d\t%s:%d:%d\t%s\n", *count, e.Name(), ln+1, col, line)
				}
			}
		} else if !e.IsDir() {
			*count++
			fmt.Fprintf(b, "%d\t%s\n", *count, subPath)
		}
		if e.IsDir() {
			walkFS(subPath, glob, pattern, ignoreCase, limit, count, b)
		}
	}
}

func matchSimpleGlob(pattern, name string) bool {
	px, nx := 0, 0
	for px < len(pattern) {
		if pattern[px] == '*' {
			if px+1 >= len(pattern) {
				return true
			}
			for ; nx < len(name); nx++ {
				if matchSimpleGlob(pattern[px+1:], name[nx:]) {
					return true
				}
			}
			return false
		}
		if nx >= len(name) {
			return false
		}
		if pattern[px] != '?' && pattern[px] != name[nx] {
			return false
		}
		px++
		nx++
	}
	return nx == len(name)
}

func matchLine(line, pattern string, ignoreCase bool) bool {
	if ignoreCase {
		return strings.Contains(strings.ToLower(line), strings.ToLower(pattern))
	}
	return strings.Contains(line, pattern)
}

func matchColumn(line, pattern string, ignoreCase bool) int {
	s, p := line, pattern
	if ignoreCase {
		s = strings.ToLower(s)
		p = strings.ToLower(p)
	}
	idx := strings.Index(s, p)
	if idx < 0 {
		return 1
	}
	return idx + 1
}

// ---- mime detection ----

func detectMime(data []byte) string {
	mimeType := http.DetectContentType(data)
	if strings.HasPrefix(mimeType, "image/") || strings.HasPrefix(mimeType, "video/") || strings.HasPrefix(mimeType, "audio/") {
		return mimeType
	}
	if mimeType == "application/octet-stream" {
		return "text/plain"
	}
	return mimeType
}

func isTextMime(mimeType string) bool {
	return strings.HasPrefix(mimeType, "text/") ||
		mimeType == "application/json" ||
		mimeType == "application/xml" ||
		mimeType == "application/javascript"
}

// Ensure mime package import used
var _ = mime.TypeByExtension
