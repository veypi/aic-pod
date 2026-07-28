package aichost

import (
	"bufio"
	"context"
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

// searchStreamThreshold 是 grep 候选文件的大小阈值：不超过则整读（保留完整
// UTF-8 校验语义），超过则嗅探前 512 字节判定文本并流式逐行匹配（内存上限控制）。
// 与 aic tools/fs 一致。
const searchStreamThreshold = 8 << 20 // 8MB

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
// 输出另受 512KB 字节预算约束（§2.5）：超预算只保留完整行并标 truncated。
// 与 aic tools/fs.fsSearch 逐行为一致。
func handleFsSearch(ctx context.Context, pa *parsedArgv) (*ToolResult, error) {
	root := pa.positional[0]
	glob := pa.flags["glob"]
	pattern := pa.flags["pattern"]
	ignoreCase := pa.bools["ignore-case"]

	rootInfo, err := os.Stat(root)
	if err != nil {
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

	matches, err := collectMatches(ctx, root, rootInfo, glob, pattern, limit+1, ignoreCase)
	if err != nil {
		return &ToolResult{Error: fmt.Sprintf("fs search: %v", err)}, nil
	}
	truncated := len(matches) > limit
	if truncated {
		matches = matches[:limit]
	}

	isGrep := pattern != ""
	attrs := map[string]string{
		"action": "search",
		"path":   root,
		"mime":   "text/plain",
	}

	if len(matches) == 0 {
		attrs["rows"] = "0"
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

	// 字节预算（§2.5）：逐行累计，超预算即停并标 truncated，只保留完整行；
	// 首行自身超预算时放行（由响应侧统一截断兜底）。
	rows := 0
	var b strings.Builder
	writeRow := func(s string) {
		if rows > 0 && b.Len()+len(s)+1 > maxContentBytes {
			truncated = true
			return
		}
		b.WriteString(s)
		b.WriteByte('\n')
		rows++
	}
	if isGrep {
		// 格式：{n}\t{完整路径}:{行号}\t{行内容}（n、行号均 1 基，与 grep -n 一致，不设列号）
		for _, m := range matches {
			n := rows
			writeRow(fmt.Sprintf("%d\t%s:%d\t%s", n+1, m.fullPath, m.lineNum, m.lineText))
			if rows == n {
				break
			}
		}
	} else {
		// 格式：{n}\t{完整路径}{目录加/}\t{size}\t{mtime_unix}
		for _, m := range matches {
			label := m.fullPath
			if m.isDir {
				label += "/"
			}
			n := rows
			writeRow(fmt.Sprintf("%d\t%s\t%d\t%d", n+1, label, m.size, m.modTime))
			if rows == n {
				break
			}
		}
	}
	attrs["rows"] = strconv.Itoa(rows)
	attrs["truncated"] = strconv.FormatBool(truncated)
	return &ToolResult{Content: strings.TrimRight(b.String(), "\n"), Attrs: attrs}, nil
}

// collectMatches 遍历 root 收集匹配项，最多 max 条。
// 结果按 mtime 倒序；grep 模式同文件内按行号升序。
// rootInfo 为调用方已 Stat 的结果，避免重复 Stat。
// 与 aic tools/fs.collectMatches 逐行为一致。
func collectMatches(ctx context.Context, root string, rootInfo os.FileInfo, glob, pattern string, max int, ignoreCase bool) ([]searchMatch, error) {
	glob = strings.TrimSpace(glob)
	if ignoreCase {
		glob = strings.ToLower(glob)
		pattern = strings.ToLower(pattern)
	}
	isGrep := pattern != ""

	var files []searchMatch
	var walk func(dir string) error
	walk = func(dir string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
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
				if err := walk(full); err != nil {
					return err
				}
				continue
			}
			if !matchName(glob, name, ignoreCase) {
				continue
			}
			if isGrep && (info == nil || !info.Mode().IsRegular()) {
				// grep 只读常规文件：跳过设备/管道/socket 等特殊文件，
				// 避免对伪文件发起可能永久阻塞的 read
				continue
			}
			files = append(files, searchMatch{fullPath: full, size: size, modTime: modTime})
		}
		return nil
	}
	// 搜索根可以是单个文件：作为唯一候选，glob 过滤器同样作用于文件名
	if !rootInfo.IsDir() {
		if matchName(glob, filepath.Base(root), ignoreCase) {
			files = append(files, searchMatch{fullPath: root, size: rootInfo.Size(), modTime: rootInfo.ModTime().Unix()})
		}
	} else if err := walk(root); err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].modTime > files[j].modTime })

	if !isGrep {
		if len(files) > max {
			files = files[:max]
		}
		return files, nil
	}

	// grep 模式：逐文件逐行做 glob 全行匹配
	var out []searchMatch
	for _, f := range files {
		if len(out) >= max {
			break
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		matches := grepFile(ctx, f.fullPath, f.size, pattern, max-len(out), ignoreCase)
		for _, m := range matches {
			out = append(out, searchMatch{fullPath: f.fullPath, lineNum: m.lineNum, lineText: m.lineText})
		}
	}
	return out, nil
}

// grepFile 对单个文件逐行做 glob 全行匹配，返回前 max 条。
// size 为调用方已知的文件大小：≤searchStreamThreshold 整读（完整 UTF-8 校验），
// 否则嗅探前 512 字节判定文本并流式读取（内存以最长行为界）。
// 行尾 \r（CRLF 文件）在匹配与输出前统一剥除。
// 读不了的文件返回 nil（与整读时代跳过行为一致）。
// ctx 到期即放弃当前文件返回已收集部分：/proc 等伪文件的 read 可能永久阻塞，
// 不能让单个文件拖垮整个请求。
func grepFile(ctx context.Context, p string, size int64, pattern string, max int, ignoreCase bool) []searchMatch {
	matchLine := func(line string) (string, bool) {
		line = strings.TrimSuffix(line, "\r")
		cmp := line
		if ignoreCase {
			cmp = strings.ToLower(line)
		}
		return line, globMatch(pattern, cmp)
	}

	if size <= searchStreamThreshold {
		// 整读在独立 goroutine 中执行：/proc 等伪文件的 read 可能永久阻塞（无 EOF），
		// ctx 到期即放弃该文件（阻塞的读 goroutine 泄漏，不拖垮请求与后续请求）
		data, ok := readFileCtx(ctx, p)
		if !ok || !isTextContent(data) {
			return nil
		}
		var out []searchMatch
		for i, line := range strings.Split(string(data), "\n") {
			if text, ok := matchLine(line); ok {
				out = append(out, searchMatch{lineNum: i + 1, lineText: text})
				if len(out) >= max {
					break
				}
			}
		}
		return out
	}

	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64<<10)
	// 嗅探前 512 字节判定文本（截断的多字节字符尾部不计入 UTF-8 校验）
	head, _ := r.Peek(512)
	for n := 0; n < 3 && len(head) > 0 && !utf8.Valid(head); n++ {
		head = head[:len(head)-1]
	}
	if !isTextContent(head) {
		return nil
	}
	var out []searchMatch
	lineNum := 0
	for {
		if ctx.Err() != nil {
			return out // ctx 到期：返回已收集部分
		}
		line, err := r.ReadString('\n')
		if len(line) > 0 {
			lineNum++
			line = strings.TrimSuffix(line, "\n")
			if text, ok := matchLine(line); ok {
				out = append(out, searchMatch{lineNum: lineNum, lineText: text})
				if len(out) >= max {
					return out
				}
			}
		}
		if err != nil {
			return out // EOF 或读中断：返回已收集部分
		}
	}
}

// readFileCtx 整读文件，ctx 到期放弃并返回 ok=false。
// 用于防御伪文件（/proc/kmsg 等）上的永久阻塞 read：放弃时读 goroutine 泄漏，
// 但不阻塞调用方。仅适合小文件（≤searchStreamThreshold）路径。
func readFileCtx(ctx context.Context, p string) ([]byte, bool) {
	type readResult struct {
		data []byte
		err  error
	}
	ch := make(chan readResult, 1)
	go func() {
		data, err := os.ReadFile(p)
		ch <- readResult{data, err}
	}()
	select {
	case <-ctx.Done():
		return nil, false
	case r := <-ch:
		if r.err != nil {
			return nil, false
		}
		return r.data, true
	}
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
// 线性双指针实现：O(len(s)) 时间、O(1) 额外空间（s 指针单调不回退），
// 语义与 DP 实现等价（已有测试向量锁定）。
// 与 aic tools/fs.globMatch 逐行为一致。
func globMatch(pattern, s string) bool {
	p, t := []rune(pattern), []rune(s)
	i, j := 0, 0      // i → t，j → p
	star, mark := -1, 0 // 最近一个 `*` 的位置与其已消费的 t 前缀长度
	for i < len(t) {
		switch {
		case j < len(p) && (p[j] == '?' || p[j] == t[i]):
			i++
			j++
		case j < len(p) && p[j] == '*':
			star, mark = j, i
			j++
		case star >= 0:
			// 回退到最近的 `*`：让它多消费一个字符
			mark++
			i = mark
			j = star + 1
		default:
			return false
		}
	}
	for j < len(p) && p[j] == '*' {
		j++
	}
	return j == len(p)
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
