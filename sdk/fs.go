package aicenv

import (
	"context"
	"encoding/base64"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
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
// action 指定操作类型，argv 为位置路径参数加 --flag 标志参数，与 aic ufs 工具语义一致。
func FsTool() Tool {
	return Tool{
		Def: ToolDef{
			Name: "fs",
			Description: "Purpose: Read and modify files in the execution environment filesystem. " +
				"Actions and their argv examples:\n" +
				`  {"action": "ls",     "argv": ["/tmp"]}` + "\n" +
				`  {"action": "read",   "argv": ["/app/log.txt", "--offset", "100", "--limit", "200"]}` + "\n" +
				`  {"action": "write",  "argv": ["/app/notes.txt", "--content", "hello"]}` + "\n" +
				`  {"action": "edit",   "argv": ["/app/index.html", "--old", "<title>Old</title>", "--new", "<title>New</title>"]}` + "\n" +
				`  {"action": "edit",   "argv": ["/app/foo.txt", "--old", "x", "--new", "y", "--replace-all"]}` + "\n" +
				`  {"action": "rm",     "argv": ["/tmp/junk.txt"]}` + "\n" +
				`  {"action": "mkdir",  "argv": ["/app/subdir"]}` + "\n" +
				`  {"action": "cp",     "argv": ["/app/a.txt", "/tmp/b.txt"]}` + "\n" +
				`  {"action": "mv",     "argv": ["/app/old.txt", "/app/new.txt"]}` + "\n" +
				`  {"action": "search", "argv": ["/app", "--glob", "*.md", "--pattern", "TODO", "--limit", "20"]}` + "\n" +
				"Important rules: write overwrites the whole file; edit requires exact --old value; " +
				"read returns at most 1000 lines, use --offset/--limit to page; " +
				"read on image files (png/jpg/gif/webp) returns viewable image_data, oversized images are auto-compressed to jpeg; " +
				"search without --pattern lists files with size and mtime; " +
				"cp and mv fail if the destination already exists.",
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
	pa := parseArgv(params.Argv)

	action := strings.ToLower(params.Action)
	if action == "" {
		action = inferFsAction(pa)
	}
	if action == "" {
		return &ToolResult{Error: "fs: action is required (supported: ls, read, write, edit, rm, mkdir, cp, mv, search)"}, nil
	}
	if len(pa.positional) == 0 {
		return &ToolResult{Error: "fs: argv requires at least one path argument"}, nil
	}
	if (action == "cp" || action == "mv") && len(pa.positional) < 2 {
		return &ToolResult{Error: fmt.Sprintf("fs %s: source and destination paths required", action)}, nil
	}

	switch action {
	case "read":
		return handleFsRead(pa)
	case "write":
		return handleFsWrite(pa)
	case "edit":
		return handleFsEdit(pa)
	case "ls":
		return handleFsLs(pa)
	case "rm":
		return handleFsRemove(pa)
	case "mkdir":
		return handleFsMkdir(pa)
	case "cp":
		return handleFsCopy(pa)
	case "mv":
		return handleFsMove(pa)
	case "search":
		return handleFsSearch(pa)
	default:
		return &ToolResult{
			Error: fmt.Sprintf("fs: unknown action %q (supported: ls, read, write, edit, rm, mkdir, cp, mv, search)", params.Action),
		}, nil
	}
}

// ---- fs handlers ----

func handleFsRead(pa *parsedArgv) (*ToolResult, error) {
	offset := pa.getInt("offset")
	limit := pa.getInt("limit")
	if offset < 0 {
		return &ToolResult{Error: fmt.Sprintf("fs read: offset must be >= 0, got %d", offset)}, nil
	}
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}

	path := pa.positional[0]
	data, err := os.ReadFile(path)
	if err != nil {
		return &ToolResult{Error: err.Error()}, nil
	}

	mimeType := detectMime(data, path)
	if !isTextMime(mimeType) && utf8.Valid(data) {
		// 类型无法识别但内容是合法 UTF-8，按纯文本处理
		mimeType = "text/plain"
	}
	attrs := map[string]string{
		"action": "read",
		"path":   path,
		"mime":   mimeType,
	}

	if !isTextMime(mimeType) {
		attrs["size"] = strconv.Itoa(len(data))
		if isViewableImage(mimeType) {
			w, h := imageDimensions(data)
			imgData, imgMime := data, mimeType
			if len(data) > imageDataMaxBytes {
				c, err := compressImage(data, mimeType)
				if err != nil {
					return &ToolResult{
						Content: fmt.Sprintf("Image file too large to view: %s (%s, %dx%d, %d bytes, auto-compress failed: %v)", path, mimeType, w, h, len(data), err),
						Attrs:   attrs,
					}, nil
				}
				imgData, imgMime = c.data, "image/jpeg"
				// 与服务端 extractToolImages 的 image_compressed 格式一致（避免 ">" 破坏 tool_result XML 解析）
				attrs["image_compressed"] = fmt.Sprintf("%d bytes → image/jpeg %dx%d quality %d (%d bytes)", len(data), c.width, c.height, c.quality, len(c.data))
			}
			attrs["image_data"] = "data:" + imgMime + ";base64," + base64.StdEncoding.EncodeToString(imgData)
			return &ToolResult{
				Content: fmt.Sprintf("Image file: %s (%s, %dx%d, %d bytes)", path, mimeType, w, h, len(data)),
				Attrs:   attrs,
			}, nil
		}
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

func handleFsWrite(pa *parsedArgv) (*ToolResult, error) {
	path := pa.positional[0]
	content := pa.flags["content"]
	if content == "" && len(pa.positional) > 1 {
		content = pa.positional[1]
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return &ToolResult{Error: err.Error()}, nil
	}
	lines := countLines(content)
	return &ToolResult{
		Content: fmt.Sprintf("wrote file: %s (%d lines, %d bytes)", path, lines, len(content)),
		Attrs: map[string]string{
			"action": "write",
			"path":   path,
			"lines":  strconv.Itoa(lines),
			"bytes":  strconv.Itoa(len(content)),
		},
	}, nil
}

func handleFsEdit(pa *parsedArgv) (*ToolResult, error) {
	path := pa.positional[0]
	oldStr, newStr := pa.flags["old"], pa.flags["new"]
	replaceAll := pa.bools["replace-all"]
	if oldStr == "" {
		return &ToolResult{Error: "fs edit: --old is required"}, nil
	}
	if oldStr == newStr {
		return &ToolResult{Error: "fs edit: --new must be different from --old"}, nil
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
		Content: fmt.Sprintf("updated file: %s", path),
		Attrs:   map[string]string{"action": "edit", "path": path},
	}, nil
}

func handleFsLs(pa *parsedArgv) (*ToolResult, error) {
	path := pa.positional[0]
	info, err := os.Stat(path)
	if err != nil {
		return &ToolResult{Error: err.Error()}, nil
	}
	if !info.IsDir() {
		return &ToolResult{
			Content: "",
			Attrs: map[string]string{
				"action":    "ls",
				"path":      path,
				"mime":      "text/plain",
				"rows":      "0",
				"path_kind": "file",
				"truncated": "false",
			},
		}, nil
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

	return &ToolResult{
		Content: strings.TrimRight(b.String(), "\n"),
		Attrs: map[string]string{
			"action":    "ls",
			"path":      path,
			"mime":      "text/plain",
			"rows":      strconv.Itoa(len(entries)),
			"path_kind": "directory",
			"truncated": "false",
		},
	}, nil
}

func handleFsRemove(pa *parsedArgv) (*ToolResult, error) {
	path := pa.positional[0]
	info, err := os.Stat(path)
	if err != nil {
		return &ToolResult{Error: fmt.Sprintf("fs rm: %s: %v", path, err)}, nil
	}
	count := 0
	if info.IsDir() {
		count = countEntries(path)
	}
	if err := os.RemoveAll(path); err != nil {
		return &ToolResult{Error: err.Error()}, nil
	}
	msg := fmt.Sprintf("removed %s", path)
	if count > 0 {
		msg += fmt.Sprintf(" (%d items)", count)
	}
	return &ToolResult{
		Content: msg,
		Attrs:   map[string]string{"action": "rm", "path": path},
	}, nil
}

// countEntries 递归统计目录下的文件和目录总数。
func countEntries(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := len(entries)
	for _, e := range entries {
		if e.IsDir() {
			n += countEntries(filepath.Join(dir, e.Name()))
		}
	}
	return n
}

func handleFsMkdir(pa *parsedArgv) (*ToolResult, error) {
	path := pa.positional[0]
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return &ToolResult{Error: fmt.Sprintf("fs mkdir: %s already exists", path)}, nil
	}
	if err := os.MkdirAll(path, 0755); err != nil {
		return &ToolResult{Error: err.Error()}, nil
	}
	return &ToolResult{
		Content: fmt.Sprintf("created %s", path),
		Attrs:   map[string]string{"action": "mkdir", "path": path},
	}, nil
}

func handleFsCopy(pa *parsedArgv) (*ToolResult, error) {
	src, dst := pa.positional[0], pa.positional[1]
	info, err := os.Stat(src)
	if err != nil {
		return &ToolResult{Error: fmt.Sprintf("fs cp: cannot stat source %s: %v", src, err)}, nil
	}
	if _, err := os.Stat(dst); err == nil {
		return &ToolResult{Error: fmt.Sprintf("fs cp: destination %s already exists", dst)}, nil
	}
	if info.IsDir() {
		if err := os.MkdirAll(dst, 0755); err != nil {
			return &ToolResult{Error: fmt.Sprintf("fs cp: cannot create destination directory %s: %v", dst, err)}, nil
		}
		if err := copyDir(src, dst); err != nil {
			return &ToolResult{Error: err.Error()}, nil
		}
	} else {
		data, err := os.ReadFile(src)
		if err != nil {
			return &ToolResult{Error: fmt.Sprintf("fs cp: cannot read source %s: %v", src, err)}, nil
		}
		if err := os.WriteFile(dst, data, 0644); err != nil {
			return &ToolResult{Error: fmt.Sprintf("fs cp: cannot write destination %s: %v", dst, err)}, nil
		}
	}
	return &ToolResult{
		Content: fmt.Sprintf("copied %s to %s", src, dst),
		Attrs:   map[string]string{"action": "cp", "source_path": src, "path": dst},
	}, nil
}

func copyDir(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("fs cp: cannot read directory %s: %v", src, err)
	}
	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())
		if entry.IsDir() {
			if err := os.MkdirAll(dstPath, 0755); err != nil {
				return fmt.Errorf("fs cp: cannot create directory %s: %v", dstPath, err)
			}
			if err := copyDir(srcPath, dstPath); err != nil {
				return err
			}
		} else {
			data, err := os.ReadFile(srcPath)
			if err != nil {
				return fmt.Errorf("fs cp: cannot read %s: %v", srcPath, err)
			}
			if err := os.WriteFile(dstPath, data, 0644); err != nil {
				return fmt.Errorf("fs cp: cannot write %s: %v", dstPath, err)
			}
		}
	}
	return nil
}

func handleFsMove(pa *parsedArgv) (*ToolResult, error) {
	src, dst := pa.positional[0], pa.positional[1]
	if src == dst {
		return &ToolResult{Error: fmt.Sprintf("fs mv: %s and %s are identical", src, dst)}, nil
	}
	if _, err := os.Stat(dst); err == nil {
		return &ToolResult{Error: fmt.Sprintf("fs mv: destination %s already exists", dst)}, nil
	}
	if err := os.Rename(src, dst); err != nil {
		return &ToolResult{Error: fmt.Sprintf("fs mv: cannot move %s to %s: %v", src, dst, err)}, nil
	}
	return &ToolResult{
		Content: fmt.Sprintf("moved %s to %s", src, dst),
		Attrs:   map[string]string{"action": "mv", "source_path": src, "path": dst},
	}, nil
}

func handleFsSearch(pa *parsedArgv) (*ToolResult, error) {
	glob := pa.flags["glob"]
	pattern := pa.flags["pattern"]
	limit := pa.getInt("limit")
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	ignoreCase := pa.bools["ignore-case"]
	root := pa.positional[0]

	if _, err := os.Stat(root); err != nil {
		return &ToolResult{Error: fmt.Sprintf("search: %s: %v", root, err)}, nil
	}

	var b strings.Builder
	count := 0
	walkFS(root, glob, pattern, ignoreCase, limit, &count, &b)

	isGrep := pattern != ""
	if count == 0 {
		msg := fmt.Sprintf("no files matched glob %q in %s", glob, root)
		if isGrep {
			msg = fmt.Sprintf("no lines matched pattern %q in %s", pattern, root)
		}
		return &ToolResult{
			Content: msg,
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
			"mime":      "text/plain",
			"rows":      strconv.Itoa(count),
			"truncated": strconv.FormatBool(count >= limit),
		},
	}, nil
}

// countLines 统计内容行数：'\n' 的数量加上末尾无换行符的一行。空内容为 0 行。
func countLines(content string) int {
	if content == "" {
		return 0
	}
	n := strings.Count(content, "\n")
	if !strings.HasSuffix(content, "\n") {
		n++
	}
	return n
}

// ---- argv helpers ----

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

// parsedArgv 是 Unix 风格 argv 的解析结果：位置参数、--flag value、--bool-flag。
type parsedArgv struct {
	positional []string
	flags      map[string]string
	bools      map[string]bool
}

// parseArgv 将 argv 解析为位置参数与标志参数，与 aic libs/tools.ParseArgv 语义一致。
func parseArgv(argv []string) *parsedArgv {
	pa := &parsedArgv{
		flags: make(map[string]string),
		bools: make(map[string]bool),
	}
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if key, ok := strings.CutPrefix(arg, "--"); ok && looksLikeFlagName(key) {
			if i+1 < len(argv) && !looksLikeFlag(argv[i+1]) {
				pa.flags[key] = argv[i+1]
				i++
			} else {
				pa.bools[key] = true
			}
		} else {
			pa.positional = append(pa.positional, arg)
		}
	}
	return pa
}

// looksLikeFlag 判断 arg 是否为 "--name" 风格的标志。
func looksLikeFlag(arg string) bool {
	key, ok := strings.CutPrefix(arg, "--")
	return ok && looksLikeFlagName(key)
}

// looksLikeFlagName 判断 key（去掉 "--" 前缀后的部分）是否是合法标志名：
// 非空且仅由字母、数字、'-'、'_' 组成。任意文本（如 YAML front matter "---\n..."）
// 不会通过检查，因此会被当作上一个标志的值而不是新标志。
func looksLikeFlagName(key string) bool {
	if key == "" {
		return false
	}
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}

func (pa *parsedArgv) getInt(name string) int {
	v, _ := strconv.Atoi(pa.flags[name])
	return v
}

// inferFsAction 在 action 缺失时根据特征标志推断操作，与 ufs 工具一致。
func inferFsAction(pa *parsedArgv) string {
	if _, ok := pa.flags["old"]; ok {
		return "edit"
	}
	if pa.bools["replace-all"] {
		return "edit"
	}
	if _, ok := pa.flags["content"]; ok {
		return "write"
	}
	if _, ok := pa.flags["glob"]; ok {
		return "search"
	}
	if _, ok := pa.flags["pattern"]; ok {
		return "search"
	}
	if pa.bools["ignore-case"] {
		return "search"
	}
	if _, ok := pa.flags["offset"]; ok {
		return "read"
	}
	if _, ok := pa.flags["limit"]; ok {
		return "read"
	}
	return ""
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
		if glob != "" && !matchSimpleGlob(glob, e.Name()) {
			if e.IsDir() {
				walkFS(subPath, glob, pattern, ignoreCase, limit, count, b)
			}
			continue
		}
		if pattern != "" {
			if !e.IsDir() {
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
						fmt.Fprintf(b, "%d\t%s:%d:%d\t%s\n", *count, subPath, ln+1, col, line)
					}
				}
			}
		} else {
			*count++
			label := subPath
			if e.IsDir() {
				label += "/"
			}
			var size, mtime int64
			if info, err := e.Info(); err == nil {
				size, mtime = info.Size(), info.ModTime().Unix()
			}
			fmt.Fprintf(b, "%d\t%s\t%d\t%d\n", *count, label, size, mtime)
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

// detectMime 检测文件的裸 media type（不含 charset 参数）。
// application/octet-stream 按扩展名细化，与 aic libstools 一致。
func detectMime(data []byte, path string) string {
	mimeType, _, _ := strings.Cut(http.DetectContentType(data), ";")
	if mimeType != "application/octet-stream" {
		return mimeType
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".svg":
		return "image/svg+xml"
	case ".pdf":
		return "application/pdf"
	case ".mp4":
		return "video/mp4"
	case ".mp3":
		return "audio/mpeg"
	case ".wav":
		return "audio/wav"
	case ".zip":
		return "application/zip"
	case ".tar", ".gz", ".tgz":
		return "application/gzip"
	}
	return mimeType
}

// isViewableImage、imageDataMaxBytes 等图片处理逻辑见 image.go

func isTextMime(mimeType string) bool {
	return strings.HasPrefix(mimeType, "text/") ||
		mimeType == "application/json" ||
		mimeType == "application/xml" ||
		mimeType == "application/javascript"
}

// Ensure mime package import used
var _ = mime.TypeByExtension
