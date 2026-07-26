package aichost

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// ParseToolParams 从工具请求的 toolData 解析标准 action + argv 参数。
// argv 元素类型容错（§1.2）：number/bool 强转字符串；null/对象/数组报错。
func ParseToolParams(toolData any) (*ToolParams, error) {
	m, ok := toolData.(map[string]any)
	if !ok {
		return &ToolParams{}, nil
	}
	action, _ := m["action"].(string)
	argv, err := toStringSlice(m["argv"])
	if err != nil {
		return nil, err
	}
	return &ToolParams{Action: strings.TrimSpace(action), Argv: argv}, nil
}

func toStringSlice(v any) ([]string, error) {
	arr, ok := v.([]any)
	if !ok {
		if s, ok := v.([]string); ok {
			return s, nil
		}
		return nil, nil
	}
	out := make([]string, 0, len(arr))
	for i, item := range arr {
		switch val := item.(type) {
		case string:
			out = append(out, val)
		case float64:
			out = append(out, strconv.FormatFloat(val, 'f', -1, 64))
		case bool:
			out = append(out, strconv.FormatBool(val))
		default:
			return nil, fmt.Errorf("argv element %d must be a string", i)
		}
	}
	return out, nil
}

// fsActions 与 fsFlagSets 是 fs 指令集的 action 全集与 action 级 flag 表（§4.2/§2.1），
// 与 aic tools/fs 完全一致。
var fsActions = []string{"ls", "read", "write", "edit", "rm", "mkdir", "cp", "mv", "search", "download"}

var fsFlagSets = map[string]flagSet{
	"ls":       {},
	"read":     {"offset": flagValue, "limit": flagValue},
	"write":    {"content": flagValue, "append": flagBool},
	"edit":     {"old": flagValue, "new": flagValue, "replace-all": flagBool},
	"rm":       {},
	"mkdir":    {},
	"cp":       {},
	"mv":       {},
	"search":   {"glob": flagValue, "pattern": flagValue, "limit": flagValue, "ignore-case": flagBool},
	"download": {"from": flagValue, "max-size": flagValue},
}

// FsTool 返回内置 fs 工具（§4：两端行为一致，aic tools/fs 同源语义）。
func FsTool() Tool {
	return Tool{
		Def: ToolDef{
			Name: "fs",
			Description: "Purpose: Read and modify files in the host filesystem. " +
				"Actions and their argv examples:\n" +
				`  {"action": "ls",     "argv": ["/tmp"]}` + "\n" +
				`  {"action": "read",   "argv": ["/app/log.txt", "--offset", "100", "--limit", "200"]}` + "\n" +
				`  {"action": "write",  "argv": ["/app/notes.txt", "--content", "hello"]}` + "\n" +
				`  {"action": "write",  "argv": ["/app/log.txt", "--content", "more", "--append"]}` + "\n" +
				`  {"action": "edit",   "argv": ["/app/foo.txt", "--old", "x", "--new", "y", "--replace-all"]}` + "\n" +
				`  {"action": "rm",     "argv": ["/tmp/junk.txt"]}` + "\n" +
				`  {"action": "mkdir",  "argv": ["/app/subdir"]}` + "\n" +
				`  {"action": "cp",     "argv": ["/app/a.txt", "/tmp/b.txt"]}` + "\n" +
				`  {"action": "mv",     "argv": ["/app/old.txt", "/app/new.txt"]}` + "\n" +
				`  {"action": "search", "argv": ["/app", "--glob", "*.md", "--pattern", "*TODO*", "--limit", "20"]}` + "\n" +
				`  {"action": "download", "argv": ["/tmp/model.bin", "--from", "https://example.com/model.bin"]}` + "\n" +
				"Important rules: write overwrites the whole file unless --append; edit requires exact unique --old value; " +
				"read returns at most 1000 lines (1-based --offset/--limit paging); " +
				"read on image files (png/jpg/gif/webp) returns viewable image_data, oversized images are auto-compressed to jpeg; " +
				"search --pattern is a full-line glob (use *text* for substring match); " +
				"cp, mv and download fail if the destination already exists.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"action": map[string]any{
						"type": "string",
						"enum": fsActions,
					},
					"argv": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				},
				"required": []string{"action", "argv"},
			},
			// §10.2：rm 显式声明 action 级等级，其余字符串简写继承基线 1
			Actions: []any{
				ActionLevel{Name: "rm", RequiredLevel: 2},
				"ls", "read", "write", "edit", "mkdir", "cp", "mv", "search", "download",
			},
			RequiredLevel: 1,
			PolicyVersion: "1",
		},
		Handler: func(ctx context.Context, data any) (*ToolResult, error) {
			params, err := ParseToolParams(data)
			if err != nil {
				return &ToolResult{Error: "fs: " + err.Error()}, nil
			}
			return handleFs(ctx, params)
		},
	}
}

func handleFs(_ context.Context, params *ToolParams) (*ToolResult, error) {
	action := strings.ToLower(params.Action)
	if action == "" {
		// §4.4：不做特征 flag 推断——无害报错，让 Agent 显式修正后重试
		return &ToolResult{Error: fmt.Sprintf("fs: action is required (supported: %s)", strings.Join(fsActions, ", "))}, nil
	}
	flags, ok := fsFlagSets[action]
	if !ok {
		return &ToolResult{Error: fmt.Sprintf("fs: unknown action %q (supported: %s)", params.Action, strings.Join(fsActions, ", "))}, nil
	}

	pa, err := parseActionArgv("fs", action, params.Argv, flags)
	if err != nil {
		return &ToolResult{Error: err.Error()}, nil
	}
	if len(pa.positional) == 0 {
		return &ToolResult{Error: fmt.Sprintf("fs %s: path is required", action)}, nil
	}
	if (action == "cp" || action == "mv") && len(pa.positional) < 2 {
		return &ToolResult{Error: fmt.Sprintf("fs %s: source and destination paths are required", action)}, nil
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
	case "download":
		return handleFsDownload(pa)
	}
	return &ToolResult{Error: fmt.Sprintf("fs: unknown action %q (supported: %s)", action, strings.Join(fsActions, ", "))}, nil
}

// ---- read ----

func handleFsRead(pa *parsedArgv) (*ToolResult, error) {
	// §4.3：--offset/--limit 均 1 基，与 Content 行号同一编号空间
	offset := 1
	if v, ok := pa.flags["offset"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return &ToolResult{Error: fmt.Sprintf("fs read: offset must be >= 1, got %s", v)}, nil
		}
		offset = n
	}
	limit := 1000
	if v, ok := pa.flags["limit"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return &ToolResult{Error: fmt.Sprintf("fs read: limit must be >= 1, got %s", v)}, nil
		}
		if n < 1000 {
			limit = n
		}
	}

	path := pa.positional[0]
	data, err := os.ReadFile(path)
	if err != nil {
		return &ToolResult{Error: fmt.Sprintf("fs read: %v", err)}, nil
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
						Error: fmt.Sprintf("fs read: image too large even after compression (%d bytes)", len(data)),
						Attrs: attrs,
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

	lines := strings.Split(string(data), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	total := len(lines)
	if offset > total {
		return &ToolResult{Error: fmt.Sprintf("fs read: offset %d exceeds %d lines", offset, total)}, nil
	}
	end := offset - 1 + limit
	if end > total {
		end = total
	}
	truncated := end < total

	var b strings.Builder
	for i := offset - 1; i < end; i++ {
		fmt.Fprintf(&b, "%d\t%s\n", i+1, lines[i])
	}
	body := b.String()

	// §4.3：512KB 内容上限先于 --limit 触发时只保留完整行，rows/range 同步收紧
	if cut, wasCut := truncateContentBytes(body, maxContentBytes); wasCut {
		body = cut
		kept := strings.Count(body, "\n")
		end = offset - 1 + kept
		truncated = true
	}

	attrs["total_lines"] = strconv.Itoa(total)
	attrs["rows"] = strconv.Itoa(end - (offset - 1))
	attrs["range"] = fmt.Sprintf("%d-%d", offset, end)
	attrs["truncated"] = strconv.FormatBool(truncated)
	return &ToolResult{Content: body, Attrs: attrs}, nil
}

// ---- write ----

func handleFsWrite(pa *parsedArgv) (*ToolResult, error) {
	path := pa.positional[0]
	content, ok := pa.flags["content"]
	if !ok {
		// §4.3：--content 必填，不做位置参数兜底
		return &ToolResult{Error: "fs write: --content is required"}, nil
	}
	if pa.bools["append"] {
		// 显式追加模式（文件不存在则创建）；追加不幂等——超时后先 read 确认尾部再重试
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return &ToolResult{Error: fmt.Sprintf("fs write: %v", err)}, nil
		}
		_, werr := f.WriteString(content)
		cerr := f.Close()
		if werr != nil {
			return &ToolResult{Error: fmt.Sprintf("fs write: %v", werr)}, nil
		}
		if cerr != nil {
			return &ToolResult{Error: fmt.Sprintf("fs write: %v", cerr)}, nil
		}
		lines := countLines(content)
		return &ToolResult{
			Content: fmt.Sprintf("appended to %s (+%d lines, +%d bytes)", path, lines, len(content)),
			Attrs: map[string]string{
				"action": "write",
				"path":   path,
				"mode":   "append",
				"lines":  strconv.Itoa(lines),
				"bytes":  strconv.Itoa(len(content)),
			},
		}, nil
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return &ToolResult{Error: fmt.Sprintf("fs write: %v", err)}, nil
	}
	lines := countLines(content)
	return &ToolResult{
		Content: fmt.Sprintf("wrote file: %s (%d lines, %d bytes)", path, lines, len(content)),
		Attrs: map[string]string{
			"action": "write",
			"path":   path,
			"mode":   "overwrite",
			"lines":  strconv.Itoa(lines),
			"bytes":  strconv.Itoa(len(content)),
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

// ---- edit ----

func handleFsEdit(pa *parsedArgv) (*ToolResult, error) {
	path := pa.positional[0]
	oldStr, ok := pa.flags["old"]
	if !ok || oldStr == "" {
		return &ToolResult{Error: "fs edit: --old is required"}, nil
	}
	newStr := pa.flags["new"]
	if newStr == oldStr {
		return &ToolResult{Error: "fs edit: --new must be different from --old"}, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return &ToolResult{Error: fmt.Sprintf("fs edit: %v", err)}, nil
	}
	if !isTextMime(detectMime(data, path)) && !utf8.Valid(data) {
		return &ToolResult{Error: fmt.Sprintf("fs edit: %s is not a text file", path)}, nil
	}
	content := string(data)
	matches := strings.Count(content, oldStr)
	if matches == 0 {
		return &ToolResult{Error: "fs edit: --old string not found in file"}, nil
	}
	if matches > 1 && !pa.bools["replace-all"] {
		return &ToolResult{Error: fmt.Sprintf("fs edit: --old matches %d locations; provide more surrounding context to make it unique, or use --replace-all", matches)}, nil
	}
	if pa.bools["replace-all"] {
		content = strings.ReplaceAll(content, oldStr, newStr)
	} else {
		content = strings.Replace(content, oldStr, newStr, 1)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return &ToolResult{Error: fmt.Sprintf("fs edit: %v", err)}, nil
	}
	return &ToolResult{
		Content: fmt.Sprintf("updated file: %s", path),
		Attrs:   map[string]string{"action": "edit", "path": path},
	}, nil
}

// ---- ls ----

func handleFsLs(pa *parsedArgv) (*ToolResult, error) {
	path := pa.positional[0]
	info, err := os.Stat(path)
	if err != nil {
		return &ToolResult{Error: fmt.Sprintf("fs ls: %v", err)}, nil
	}
	if !info.IsDir() {
		// ls 单个文件：Content 为空字符串（§2.2 豁免）
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

	entries, err := os.ReadDir(path) // os.ReadDir 按文件名排序（UTF-8 字节序，§4.3）
	if err != nil {
		return &ToolResult{Error: fmt.Sprintf("fs ls: %v", err)}, nil
	}

	content := ""
	if len(entries) == 0 {
		content = fmt.Sprintf("empty directory: %s", path)
	} else {
		var b strings.Builder
		for i, e := range entries {
			label := e.Name()
			if e.IsDir() {
				label += "/"
			}
			fmt.Fprintf(&b, "%d\t%s\n", i+1, label)
		}
		content = strings.TrimRight(b.String(), "\n")
	}

	return &ToolResult{
		Content: content,
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

// ---- rm ----

func handleFsRemove(pa *parsedArgv) (*ToolResult, error) {
	path := pa.positional[0]
	// §4.3 根目录硬保护（拒绝，不可审批绕过）：文件系统根（/、盘符根如 C:\）禁止删除
	if isFilesystemRoot(path) {
		return &ToolResult{
			State: "rejected",
			Error: fmt.Sprintf("fs rm: cannot remove root directory %s", path),
		}, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return &ToolResult{Error: fmt.Sprintf("fs rm: %s: %v", path, err)}, nil
	}
	count := 0
	if info.IsDir() {
		count = countEntries(path)
	}
	if err := os.RemoveAll(path); err != nil {
		return &ToolResult{Error: fmt.Sprintf("fs rm: %v", err)}, nil
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

// isFilesystemRoot 判定路径是否为文件系统根（/、盘符根如 C:\、UNC 根）。
func isFilesystemRoot(path string) bool {
	clean := filepath.Clean(path)
	if runtime.GOOS == "windows" {
		// C:\ 或 C:（Clean 后盘符根）
		if len(clean) == 2 && clean[1] == ':' {
			return true
		}
		if len(clean) == 3 && clean[1] == ':' && (clean[2] == '\\' || clean[2] == '/') {
			return true
		}
		return clean == `\\`
	}
	return clean == "/"
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

// ---- mkdir / cp / mv ----

func handleFsMkdir(pa *parsedArgv) (*ToolResult, error) {
	path := pa.positional[0]
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return &ToolResult{Error: fmt.Sprintf("fs mkdir: %s already exists", path)}, nil
	}
	if err := os.MkdirAll(path, 0755); err != nil {
		return &ToolResult{Error: fmt.Sprintf("fs mkdir: %v", err)}, nil
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

// ---- download ----

// downloadClient 共享下载客户端；单请求超时 10 分钟（§4.3）。
var downloadClient = &http.Client{Timeout: 10 * time.Minute}

// handleFsDownload 从 http(s) 下载文件到 host 本地（§4.3：各环境自行发起 fetch）。
// cloud:// 与 {host_id}:// 源为预留 scheme，本期报 scheme not yet supported。
func handleFsDownload(pa *parsedArgv) (*ToolResult, error) {
	dst := pa.positional[0]
	source := pa.flags["from"]
	if source == "" {
		return &ToolResult{Error: "fs download: --from is required"}, nil
	}

	// scheme 判定（§4.3，逐条顺序匹配）
	idx := strings.Index(source, "://")
	if idx < 0 {
		return &ToolResult{Error: fmt.Sprintf("fs download: invalid source %q: missing scheme (supported: http(s)://, cloud://, {host_id}://)", source)}, nil
	}
	scheme := source[:idx]
	switch scheme {
	case "http", "https":
		// 本期实现：host 由 host agent 下载写入本地路径
	case "cloud":
		return &ToolResult{Error: "fs download: scheme not yet supported: cloud"}, nil
	default:
		// 其余左段为跨 host 源（预留）；host 端无法查表校验，统一按预留处理
		return &ToolResult{Error: fmt.Sprintf("fs download: scheme not yet supported: %s", scheme)}, nil
	}

	// 目标已存在报错（与 cp/mv 一致，不覆盖）
	if _, err := os.Stat(dst); err == nil {
		return &ToolResult{Error: fmt.Sprintf("fs download: destination %s already exists", dst)}, nil
	}

	maxSizeMB := 1024
	if v, ok := pa.flags["max-size"]; ok {
		n, err := strconv.Atoi(v)
		if err == nil && n > 0 {
			maxSizeMB = n
		}
		if maxSizeMB > 10240 {
			maxSizeMB = 10240
		}
	}

	req, err := http.NewRequest(http.MethodGet, source, nil)
	if err != nil {
		return &ToolResult{Error: fmt.Sprintf("fs download: %v", err)}, nil
	}
	resp, err := downloadClient.Do(req)
	if err != nil {
		return &ToolResult{Error: fmt.Sprintf("fs download: fetch %s: %v", source, err)}, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return &ToolResult{Error: fmt.Sprintf("fs download: fetch %s returned %s", source, resp.Status)}, nil
	}

	f, err := os.Create(dst)
	if err != nil {
		return &ToolResult{Error: fmt.Sprintf("fs download: cannot create %s: %v", dst, err)}, nil
	}
	// 字节上限：超限中止并删除半成品文件
	maxBytes := int64(maxSizeMB) << 20
	n, copyErr := io.Copy(f, io.LimitReader(resp.Body, maxBytes+1))
	if closeErr := f.Close(); copyErr == nil && closeErr != nil {
		copyErr = closeErr
	}
	if n > maxBytes {
		_ = os.Remove(dst)
		return &ToolResult{Error: fmt.Sprintf("fs download: size limit exceeded (%dMB > %dMB)", n>>20, maxSizeMB)}, nil
	}
	if copyErr != nil {
		_ = os.Remove(dst)
		return &ToolResult{Error: fmt.Sprintf("fs download: write %s: %v", dst, copyErr)}, nil
	}
	return &ToolResult{
		Content: fmt.Sprintf("downloaded %s to %s (%d bytes)", source, dst, n),
		Attrs: map[string]string{
			"action": "download",
			"path":   dst,
			"source": source,
			"bytes":  strconv.FormatInt(n, 10),
		},
	}, nil
}
