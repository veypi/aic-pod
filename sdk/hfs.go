package aichost

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// hfs.go — hfs 结构化文件工具：面向前端文件管理器（浏览器经 NATS 直连），
// 与 LLM 面向的 fs 工具（行号前缀/截断/文本报告）不同，hfs 返回结构化数据：
//
//   tool_data: {"action": "...", "path": "...", ...}（结构化字段，非 argv）
//   响应约定: ls/stat/search → Content 为 JSON；read → Content 为 base64 数据块；
//             元数据（mime/size/offset/eof 等）放 Attrs。
//
// 安全模型：hfs 免签（Unsigned）。授权依赖 natsauth 的 subject 所有权——
// 仅 owner 的前端连接与 aic server 可发布到本工具的请求 subject；
// client.go 对 Unsigned 工具强制以固定等级执行，忽略信封自报的 GrantedLevel。
// action 集封闭（仅文件操作），永不开放 exec。

// hfsReadChunk 是 read 单块默认上限：pod pub 2MB（natsauth host 用户限制），
// base64 膨胀 4/3 + 信封余量后取 1MB 原始字节。
const hfsReadChunk = 1 << 20

// hfsMaxRead 是 read 总量防御上限（前端循环拉取时的单文件天花板）。
const hfsMaxRead = 64 << 20 // 64MB

// hfsLsMaxItems 是 ls 单级条目上限（防超大目录响应超过 NATS payload）。
const hfsLsMaxItems = 10000

// HfsTool 返回内置 hfs 工具（结构化协议，免签直连）。
func HfsTool() Tool {
	return Tool{
		Unsigned: true,
		Def: ToolDef{
			Name:          "hfs",
			Description:   "Structured host filesystem access for the web file manager (browser-direct over NATS). Actions: ls/read/write/rm/mkdir/cp/mv/stat/search.",
			Actions:       []any{"ls", "read", "write", "rm", "mkdir", "cp", "mv", "stat", "search"},
			RequiredLevel: 1,
			PolicyVersion: "1",
		},
		Handler: handleHfs,
	}
}

// hfsItem 与 vigo ufs.ItemEntry 同构（前端 $fs 两端对齐）。
type hfsItem struct {
	Name    string    `json:"name"`
	Dir     bool      `json:"dir"`
	Size    int64     `json:"size"`
	Mime    string    `json:"mime"`
	ModTime int64     `json:"mod_time"`
	IsRepo  bool      `json:"is_repo"`
	Items   []hfsItem `json:"items,omitempty"`
}

// hfsMatch 与 vigo ufs.SearchMatch 同构。
type hfsMatch struct {
	Path    string `json:"path"`
	IsDir   bool   `json:"is_dir"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mod_time"`
	LineNum int    `json:"line_num,omitempty"`
	Line    string `json:"line,omitempty"`
	Column  int    `json:"column,omitempty"`
}

type hfsRequest struct {
	Action     string `json:"action"`
	Path       string `json:"path"`
	To         string `json:"to"`
	Offset     int64  `json:"offset"`
	Length     int64  `json:"length"`
	DataB64    string `json:"data_b64"`
	Truncate   bool   `json:"truncate"`
	Glob       string `json:"glob"`
	Pattern    string `json:"pattern"`
	Limit      int    `json:"limit"`
	IgnoreCase bool   `json:"ignore_case"`
	Depth      int    `json:"depth"`
}

// hfsLsMaxDepth 限制 ls 递归嵌套上限（与云端 vigo ufs WithMaxDepth(5) 对齐）。
const hfsLsMaxDepth = 5

func handleHfs(ctx context.Context, data any) (*ToolResult, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return &ToolResult{Error: "hfs: invalid tool_data"}, nil
	}
	var req hfsRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return &ToolResult{Error: "hfs: invalid tool_data: " + err.Error()}, nil
	}
	if req.Action == "" {
		return &ToolResult{Error: "hfs: action is required"}, nil
	}
	if req.Path == "" {
		return &ToolResult{Error: fmt.Sprintf("hfs %s: path is required", req.Action)}, nil
	}

	switch req.Action {
	case "stat":
		return hfsStat(req)
	case "ls":
		return hfsLs(req)
	case "read":
		return hfsRead(req)
	case "write":
		return hfsWrite(req)
	case "rm":
		return hfsRm(req)
	case "mkdir":
		return hfsMkdir(req)
	case "cp":
		return hfsCp(req)
	case "mv":
		return hfsMv(req)
	case "search":
		return hfsSearch(ctx, req)
	}
	return &ToolResult{Error: fmt.Sprintf("hfs: unknown action %q", req.Action)}, nil
}

// hfsNormPath 归一化前端传来的路径：支持 ~ 展开、Windows 正斜杠。
// host 文件是用户本机磁盘，不做 jail 限制（owner 直连语义）。
func hfsNormPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			p = home + p[1:]
		}
	}
	if runtime.GOOS == "windows" {
		p = strings.ReplaceAll(p, "/", `\`)
	}
	return filepath.Clean(p)
}

func hfsErr(action, path string, err error) *ToolResult {
	return &ToolResult{Error: fmt.Sprintf("hfs %s %s: %v", action, path, err)}
}

func hfsItemOf(abs string, info os.FileInfo) hfsItem {
	item := hfsItem{
		Name:    info.Name(),
		Dir:     info.IsDir(),
		Size:    info.Size(),
		ModTime: info.ModTime().Unix(),
	}
	if info.IsDir() {
		if st, err := os.Stat(filepath.Join(abs, ".git")); err == nil && st.IsDir() {
			item.IsRepo = true
		}
	} else {
		item.Mime = mime.TypeByExtension(strings.ToLower(filepath.Ext(info.Name())))
	}
	return item
}

// ---- stat ----

func hfsStat(req hfsRequest) (*ToolResult, error) {
	p := hfsNormPath(req.Path)
	info, err := os.Stat(p)
	if err != nil {
		if os.IsNotExist(err) {
			data, _ := json.Marshal(map[string]any{"exists": false})
			return &ToolResult{Content: string(data)}, nil
		}
		return hfsErr("stat", req.Path, err), nil
	}
	item := hfsItemOf(p, info)
	data, _ := json.Marshal(struct {
		hfsItem
		Exists bool `json:"exists"`
	}{item, true})
	return &ToolResult{Content: string(data)}, nil
}

// ---- ls ----

func hfsLs(req hfsRequest) (*ToolResult, error) {
	p := hfsNormPath(req.Path)
	info, err := os.Stat(p)
	if err != nil {
		return hfsErr("ls", req.Path, err), nil
	}
	depth := req.Depth
	if depth < 1 {
		depth = 1
	}
	if depth > hfsLsMaxDepth {
		depth = hfsLsMaxDepth
	}
	truncated := false
	root := hfsLsDir(p, info, depth, &truncated)
	data, _ := json.Marshal(root)
	attrs := map[string]string{}
	// cwd 为 pod 进程工作目录（绝对路径、正斜杠）：
	// 前端文件树以相对路径操作、以绝对路径存储引用，需此值做转换。
	if cwd, absErr := filepath.Abs("."); absErr == nil {
		attrs["cwd"] = filepath.ToSlash(cwd)
	}
	if truncated {
		attrs["truncated"] = "true"
	}
	return &ToolResult{Content: string(data), Attrs: attrs}, nil
}

// hfsLsDir 递归列举目录：depth 为剩余层级，
// depth=1 只列直接子项（子目录不带 items），与 vigo buildItemTree 语义一致。
func hfsLsDir(p string, info os.FileInfo, depth int, truncated *bool) hfsItem {
	root := hfsItemOf(p, info)
	if !info.IsDir() {
		return root
	}
	entries, err := os.ReadDir(p) // 按文件名排序
	if err != nil {
		return root
	}
	if len(entries) > hfsLsMaxItems {
		entries = entries[:hfsLsMaxItems]
		*truncated = true
	}
	items := make([]hfsItem, 0, len(entries))
	for _, e := range entries {
		ei, err := e.Info()
		if err != nil {
			continue
		}
		full := filepath.Join(p, e.Name())
		if ei.IsDir() && depth > 1 {
			items = append(items, hfsLsDir(full, ei, depth-1, truncated))
		} else {
			items = append(items, hfsItemOf(full, ei))
		}
	}
	root.Items = items
	return root
}

// ---- read ----

func hfsRead(req hfsRequest) (*ToolResult, error) {
	p := hfsNormPath(req.Path)
	info, err := os.Stat(p)
	if err != nil {
		return hfsErr("read", req.Path, err), nil
	}
	if info.IsDir() {
		return &ToolResult{Error: "hfs read: " + req.Path + " is a directory"}, nil
	}
	offset := req.Offset
	if offset < 0 {
		offset = 0
	}
	length := req.Length
	if length <= 0 || length > hfsReadChunk {
		length = hfsReadChunk
	}
	if offset > info.Size() {
		offset = info.Size()
	}
	// 防御：单文件读取总量天花板（前端循环场景）
	if offset >= hfsMaxRead {
		return &ToolResult{Error: fmt.Sprintf("hfs read: file exceeds max read size (%d bytes)", hfsMaxRead)}, nil
	}

	f, err := os.Open(p)
	if err != nil {
		return hfsErr("read", req.Path, err), nil
	}
	defer f.Close()

	buf := make([]byte, length)
	n, err := f.ReadAt(buf, offset)
	if err != nil && n == 0 {
		return hfsErr("read", req.Path, err), nil
	}
	buf = buf[:n]
	eof := offset+int64(n) >= info.Size()

	attrs := map[string]string{
		"size":   fmt.Sprintf("%d", info.Size()),
		"offset": fmt.Sprintf("%d", offset),
		"eof":    fmt.Sprintf("%t", eof),
	}
	// mime 仅在首块嗅探（后续块前端沿用首块判定）
	if offset == 0 {
		sniff := buf
		if len(sniff) > 512 {
			sniff = sniff[:512]
		}
		attrs["mime"] = detectMime(sniff, p)
	}
	return &ToolResult{
		Content: base64.StdEncoding.EncodeToString(buf),
		Attrs:   attrs,
	}, nil
}

// ---- write ----

func hfsWrite(req hfsRequest) (*ToolResult, error) {
	p := hfsNormPath(req.Path)
	data, err := base64.StdEncoding.DecodeString(req.DataB64)
	if err != nil {
		return &ToolResult{Error: "hfs write: invalid data_b64: " + err.Error()}, nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return hfsErr("write", req.Path, err), nil
	}
	if req.Truncate || req.Offset == 0 {
		// 首块：创建/截断写入
		if err := os.WriteFile(p, data, 0o644); err != nil {
			return hfsErr("write", req.Path, err), nil
		}
	} else {
		f, err := os.OpenFile(p, os.O_WRONLY, 0o644)
		if err != nil {
			return hfsErr("write", req.Path, err), nil
		}
		defer f.Close()
		if _, err := f.WriteAt(data, req.Offset); err != nil {
			return hfsErr("write", req.Path, err), nil
		}
	}
	return &ToolResult{Attrs: map[string]string{"bytes": fmt.Sprintf("%d", len(data))}}, nil
}

// ---- rm / mkdir ----

func hfsRm(req hfsRequest) (*ToolResult, error) {
	p := hfsNormPath(req.Path)
	// 与 fs 工具一致的根目录硬保护
	if isFilesystemRoot(p) {
		return &ToolResult{State: "rejected", Error: "hfs rm: cannot remove root directory " + req.Path}, nil
	}
	if _, err := os.Stat(p); err != nil {
		return hfsErr("rm", req.Path, err), nil
	}
	if err := os.RemoveAll(p); err != nil {
		return hfsErr("rm", req.Path, err), nil
	}
	return &ToolResult{}, nil
}

func hfsMkdir(req hfsRequest) (*ToolResult, error) {
	p := hfsNormPath(req.Path)
	if _, err := os.Stat(p); err == nil {
		return &ToolResult{Error: "hfs mkdir: " + req.Path + " already exists"}, nil
	}
	if err := os.MkdirAll(p, 0o755); err != nil {
		return hfsErr("mkdir", req.Path, err), nil
	}
	return &ToolResult{}, nil
}

// ---- cp / mv ----

func hfsCp(req hfsRequest) (*ToolResult, error) {
	if req.To == "" {
		return &ToolResult{Error: "hfs cp: to is required"}, nil
	}
	src, dst := hfsNormPath(req.Path), hfsNormPath(req.To)
	info, err := os.Stat(src)
	if err != nil {
		return hfsErr("cp", req.Path, err), nil
	}
	if info.IsDir() && pathInside(src, dst) {
		return &ToolResult{Error: fmt.Sprintf("hfs cp: cannot copy directory %s into itself", req.Path)}, nil
	}
	if _, err := os.Stat(dst); err == nil {
		return &ToolResult{Error: "hfs cp: destination already exists: " + req.To}, nil
	}
	if info.IsDir() {
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return hfsErr("cp", req.To, err), nil
		}
		if err := copyDir(src, dst); err != nil {
			return &ToolResult{Error: err.Error()}, nil
		}
	} else {
		data, err := os.ReadFile(src)
		if err != nil {
			return hfsErr("cp", req.Path, err), nil
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return hfsErr("cp", req.To, err), nil
		}
	}
	return &ToolResult{}, nil
}

func hfsMv(req hfsRequest) (*ToolResult, error) {
	if req.To == "" {
		return &ToolResult{Error: "hfs mv: to is required"}, nil
	}
	src, dst := hfsNormPath(req.Path), hfsNormPath(req.To)
	if src == dst {
		return &ToolResult{Error: "hfs mv: source and destination are identical"}, nil
	}
	if _, err := os.Stat(src); err != nil {
		return hfsErr("mv", req.Path, err), nil
	}
	if _, err := os.Stat(dst); err == nil {
		return &ToolResult{Error: "hfs mv: destination already exists: " + req.To}, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return hfsErr("mv", req.To, err), nil
	}
	if err := os.Rename(src, dst); err != nil {
		return hfsErr("mv", req.Path, err), nil
	}
	return &ToolResult{}, nil
}

// ---- search ----

func hfsSearch(ctx context.Context, req hfsRequest) (*ToolResult, error) {
	p := hfsNormPath(req.Path)
	if req.Glob == "" && req.Pattern == "" {
		return &ToolResult{Error: "hfs search: glob or pattern required"}, nil
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	info, err := os.Stat(p)
	if err != nil {
		return hfsErr("search", req.Path, err), nil
	}
	// 与 fs 工具共用遍历/匹配实现（glob 文件名 + glob 行匹配语义）
	matches, err := collectMatches(ctx, p, info, req.Glob, req.Pattern, limit+1, req.IgnoreCase)
	if err != nil {
		return hfsErr("search", req.Path, err), nil
	}
	truncated := len(matches) > limit
	if truncated {
		matches = matches[:limit]
	}
	rows := make([]hfsMatch, 0, len(matches))
	for _, m := range matches {
		rel, err := filepath.Rel(p, m.fullPath)
		if err != nil {
			rel = m.fullPath
		}
		rows = append(rows, hfsMatch{
			Path:    filepath.ToSlash(rel),
			IsDir:   m.isDir,
			Size:    m.size,
			ModTime: m.modTime,
			LineNum: m.lineNum,
			Line:    m.lineText,
		})
	}
	data, _ := json.Marshal(rows)
	res := &ToolResult{Content: string(data)}
	if truncated {
		res.Attrs = map[string]string{"truncated": "true"}
	}
	return res, nil
}
