package aichost

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// maxContentBytes 是响应 content 上限（§2.5：512KB，与 aic libs/tools.MaxContentBytes 一致）。
const maxContentBytes = 512 << 10

// truncateContentBytes 按 §2.5 截断规则截断 content：rune 边界收刀 + 只保留完整行。
// 与 aic libs/tools.TruncateContent 一致。
func truncateContentBytes(s string, maxBytes int) (string, bool) {
	if len(s) <= maxBytes {
		return s, false
	}
	cut := maxBytes
	for cut > 0 && !utf8.ValidString(s[:cut]) {
		cut--
	}
	out := s[:cut]
	if idx := strings.LastIndexByte(out, '\n'); idx >= 0 {
		out = out[:idx+1]
	}
	return out, true
}

// skipSearchDirs 是 search 不进入的目录名（点开头目录同样跳过），与 aic tools/fs 一致。
var skipSearchDirs = map[string]bool{
	"node_modules": true, "vendor": true, "__pycache__": true,
	"bower_components": true, "dist": true, "build": true,
	"target": true, ".next": true, ".nuxt": true,
	"coverage": true, ".turbo": true, ".output": true,
}

// searchMatch 是 search 的一条结果。
type searchMatch struct {
	fullPath string
	isDir    bool
	size     int64
	modTime  int64 // 秒级 Unix 时间戳（§4.3）
	lineNum  int   // grep 模式：1 基行号
	lineText string
}

// handleFsSearch 实现 search action（§4.3）：
//   - 仅 --glob：按文件名 glob 过滤，递归列出
//   - 带 --pattern：grep 模式，pattern 为 glob（与 --glob 同一算法），对行做完整匹配
//
// 截断按 limit+1 判定：查 limit+1 条，返回前 limit 条，恰好等于 limit 不误标。
func handleFsSearch(pa *parsedArgv) (*ToolResult, error) {
	root := pa.positional[0]
	glob := pa.flags["glob"]
	pattern := pa.flags["pattern"]
	ignoreCase := pa.bools["ignore-case"]

	if _, err := os.Stat(root); err != nil {
		return &ToolResult{Error: fmt.Sprintf("fs search: %s: %v", root, err)}, nil
	}
	limit := 100
	if v, ok := pa.flags["limit"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return &ToolResult{Error: fmt.Sprintf("fs search: limit must be >= 1, got %s", v)}, nil
		}
		limit = n
		if limit > 500 {
			limit = 500
		}
	}

	matches := collectMatches(root, glob, pattern, limit+1, ignoreCase)
	truncated := len(matches) > limit
	if truncated {
		matches = matches[:limit]
	}

	isGrep := pattern != ""
	attrs := map[string]string{
		"action": "search",
		"path":   root,
		"mime":   "text/plain",
		"rows":   strconv.Itoa(len(matches)),
	}

	if len(matches) == 0 {
		attrs["truncated"] = "false"
		if isGrep {
			hint := ""
			if !strings.ContainsAny(pattern, "*?") {
				hint = ` (pattern is full-line glob; use "*text*" for substring match)`
			}
			return &ToolResult{
				Content: fmt.Sprintf("no lines matched pattern %q in %s%s", pattern, root, hint),
				Attrs:   attrs,
			}, nil
		}
		return &ToolResult{
			Content: fmt.Sprintf("no files matched glob %q in %s", glob, root),
			Attrs:   attrs,
		}, nil
	}

	attrs["truncated"] = strconv.FormatBool(truncated)
	var b strings.Builder
	if isGrep {
		// 格式：{n}\t{完整路径}:{行号}\t{行内容}（n、行号均 1 基，与 grep -n 一致，不设列号）
		for i, m := range matches {
			fmt.Fprintf(&b, "%d\t%s:%d\t%s\n", i+1, m.fullPath, m.lineNum, m.lineText)
		}
	} else {
		// 格式：{n}\t{完整路径}{目录加/}\t{size}\t{mtime_unix}
		for i, m := range matches {
			label := m.fullPath
			if m.isDir {
				label += "/"
			}
			fmt.Fprintf(&b, "%d\t%s\t%d\t%d\n", i+1, label, m.size, m.modTime)
		}
	}
	return &ToolResult{Content: strings.TrimRight(b.String(), "\n"), Attrs: attrs}, nil
}

// collectMatches 遍历 root 收集匹配项，最多 max 条。
// 结果按 mtime 倒序；grep 模式同文件内按行号升序。
func collectMatches(root, glob, pattern string, max int, ignoreCase bool) []searchMatch {
	if ignoreCase {
		glob = strings.ToLower(glob)
		pattern = strings.ToLower(pattern)
	}
	isGrep := pattern != ""

	var files []searchMatch
	var walk func(dir string)
	walk = func(dir string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			name := e.Name()
			if strings.HasPrefix(name, ".") || (e.IsDir() && skipSearchDirs[name]) {
				continue
			}
			full := filepath.Join(dir, name)
			info, _ := e.Info()
			var modTime, size int64
			if info != nil {
				modTime = info.ModTime().Unix()
				size = info.Size()
			}
			if e.IsDir() {
				if !isGrep && matchName(glob, name, ignoreCase) {
					files = append(files, searchMatch{fullPath: full, isDir: true, modTime: modTime})
				}
				walk(full)
				continue
			}
			if !matchName(glob, name, ignoreCase) {
				continue
			}
			files = append(files, searchMatch{fullPath: full, size: size, modTime: modTime})
		}
	}
	walk(root)
	sort.Slice(files, func(i, j int) bool { return files[i].modTime > files[j].modTime })

	if !isGrep {
		if len(files) > max {
			files = files[:max]
		}
		return files
	}

	// grep 模式：逐文件逐行做 glob 全行匹配
	var out []searchMatch
	for _, f := range files {
		if len(out) >= max {
			break
		}
		data, err := os.ReadFile(f.fullPath)
		if err != nil || !isTextContent(data) {
			continue
		}
		for i, line := range strings.Split(string(data), "\n") {
			cmp := line
			if ignoreCase {
				cmp = strings.ToLower(line)
			}
			if globMatch(pattern, cmp) {
				out = append(out, searchMatch{fullPath: f.fullPath, lineNum: i + 1, lineText: line})
				if len(out) >= max {
					break
				}
			}
		}
	}
	return out
}

// matchName 按 glob 匹配文件名；glob 为空匹配一切。
func matchName(glob, name string, ignoreCase bool) bool {
	if glob == "" {
		return true
	}
	if ignoreCase {
		name = strings.ToLower(name)
	}
	return globMatch(glob, name)
}

// globMatch 是 fs search 的统一 glob 算法（§4.3：--glob 与 --pattern 同一算法，
// 两端各自以同一算法实现，无包装、无降级、无中间胶水）：
// 支持 `*`（任意序列）与 `?`（单字符），对输入做完整匹配。
// 与 aic tools/fs.globMatch 逐行为一致。
func globMatch(pattern, s string) bool {
	p, s2 := []rune(pattern), []rune(s)
	dp := make([][]bool, len(p)+1)
	for i := range dp {
		dp[i] = make([]bool, len(s2)+1)
	}
	dp[0][0] = true
	for i := 1; i <= len(p); i++ {
		if p[i-1] == '*' {
			dp[i][0] = dp[i-1][0]
		}
	}
	for i := 1; i <= len(p); i++ {
		for j := 1; j <= len(s2); j++ {
			switch p[i-1] {
			case '*':
				dp[i][j] = dp[i-1][j] || dp[i][j-1]
			case '?':
				dp[i][j] = dp[i-1][j-1]
			default:
				dp[i][j] = p[i-1] == s2[j-1] && dp[i-1][j-1]
			}
		}
	}
	return dp[len(p)][len(s2)]
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

func isTextMime(mimeType string) bool {
	return strings.HasPrefix(mimeType, "text/") ||
		mimeType == "application/json" ||
		mimeType == "application/xml" ||
		mimeType == "application/javascript"
}

// isTextContent 判定内容是否按文本处理：嗅探为文本，或无法识别但为合法 UTF-8。
// 与 aic libstools.IsTextContent 一致。
func isTextContent(data []byte) bool {
	if len(data) == 0 {
		return true
	}
	if isTextMime(detectMime(data, "")) {
		return true
	}
	return utf8.Valid(data)
}
