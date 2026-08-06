package vcore

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/veypi/aic-pod/libs/proto"
)

// Result 是指令输出（§2.2）：Content 为文本正文，Attrs 为结构化元数据。
type Result struct {
	Content string
	Attrs   map[string]string
}

func newResult(action, path string) *Result {
	r := &Result{Attrs: map[string]string{"action": action}}
	if path != "" {
		r.Attrs["path"] = path
	}
	return r
}

func (r *Result) set(k string, v any) {
	r.Attrs[k] = fmt.Sprint(v)
}

// ---- 上限（§2.5） ----

const (
	// MaxContentBytes 是 content 响应上限（上下文体积控制的有意取舍，双端一致）。
	MaxContentBytes = 512 << 10 // 512KB
	// streamThreshold 是 read/grep 整读的大小上限：超过则嗅探前 512 字节
	// 判定文本并流式按行扫描（§4.2 大文件流式化，三端一致）。
	streamThreshold = 8 << 20 // 8MB
)

// truncateContent 按 §2.5 截断规则截断 content：
// 按字节截断时在 rune 边界处收刀（不切断多字节字符）；
// 行格式下只保留完整行（丢弃末尾被切断的半行）。
func truncateContent(s string, maxBytes int) (string, bool) {
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

// ---- MIME / 文本判定（§4.2） ----

// isTextContent 判定内容是否为文本：嗅探为文本类型，或内容为合法 UTF-8。
func isTextContent(data []byte) bool {
	if len(data) == 0 {
		return true
	}
	mime := http.DetectContentType(data)
	if strings.HasPrefix(mime, "text/") || mime == "application/json" ||
		mime == "application/xml" || mime == "application/javascript" {
		return true
	}
	return utf8.Valid(data)
}

// detectMIME 检测 media type：application/octet-stream 按扩展名细化（§4.2）。
// data 取前 512 字节即可。
func detectMIME(data []byte, path string) string {
	mime := http.DetectContentType(data)
	if mime == "application/octet-stream" {
		switch strings.ToLower(filepath.Ext(path)) {
		case ".png":
			mime = "image/png"
		case ".jpg", ".jpeg":
			mime = "image/jpeg"
		case ".gif":
			mime = "image/gif"
		case ".webp":
			mime = "image/webp"
		case ".svg":
			mime = "image/svg+xml"
		case ".pdf":
			mime = "application/pdf"
		case ".mp4":
			mime = "video/mp4"
		case ".mp3":
			mime = "audio/mpeg"
		case ".wav":
			mime = "audio/wav"
		case ".zip":
			mime = "application/zip"
		case ".tar", ".gz", ".tgz":
			mime = "application/gzip"
		}
	}
	return mime
}

// isViewableImageMime 判定图片格式是否可被模型直接查看（§2.2）。
func isViewableImageMime(mime string) bool {
	switch mime {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	}
	return false
}

// ---- 错误构造（格式锁定，§2.3/§5.4） ----

// execErr 构造虚拟指令错误：消息为 {cmd}: {原因}（§5.4 基准，不带 exec 前缀——
// 指令名本身即 action）。
func execErr(cmd, format string, args ...any) *proto.ExecError {
	return &proto.ExecError{Action: cmd, Reason: fmt.Sprintf(format, args...)}
}

// fsErr 构造 fs 错误：消息为 fs {action}: {原因}（§2.3）。
func fsErr(action, format string, args ...any) *proto.ExecError {
	return &proto.ExecError{Tool: proto.ToolFS, Action: action, Reason: fmt.Sprintf(format, args...)}
}
