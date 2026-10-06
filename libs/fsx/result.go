package fsx

import (
	"fmt"
	"hash/fnv"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Result 是指令输出（§2.2）：Content 为文本正文，Attrs 为结构化元数据。
// json tag 是 RTC 直连帧协议的线上契约（libs/rtc fschan 直接 json.Marshal
// 本类型）——页面端按小写键解析；缺 tag 时 Go 产出大写键，页面静默丢空
// （2026-09-10 实网事故：Go 侧消费方反序列化大小写不敏感，仅 JS 侧可见）。
type Result struct {
	Content string            `json:"content"`
	Attrs   map[string]string `json:"attrs,omitempty"`
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

// contentVersion 返回内容的 FNV-1a 64bit 十六进制（写后版本号；供 P3
// baseVersion 校验取用，JS 端为 BigInt 等价实现）。
func contentVersion(s string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return strconv.FormatUint(h.Sum64(), 16)
}

// ---- 上限（§2.5） ----

const (
	// MaxContentBytes 是 content 响应上限（上下文体积控制的有意取舍，双端一致）。
	MaxContentBytes = 128 << 10 // 128KB
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

// ---- 错误构造（格式锁定，§2.3/§5.4） ----

// fsErr 构造 fs 错误：消息为 fs {action}: {原因}（§2.3 格式锁定；
// action 空时退化为 fs: {原因}）。批次 2 错误模型收敛：ExecError/ApprovalError
// 历史包装已删（无生产者与消费者），具体错误在所属实现构造，跨进程统一由
// Fault 表达。
func fsErr(action, format string, args ...any) error {
	reason := fmt.Sprintf(format, args...)
	if action == "" {
		return fmt.Errorf("fs: %s", reason)
	}
	return fmt.Errorf("fs %s: %s", action, reason)
}

// fsOpErr 包装底层 FS/OS 操作的错误：Error() 与 fsErr(action, "%s", err) 逐字一致
// （"fs <action>: <原因>"），但保留错误链（errors.Is/As 可达）——pod 侧据此把
// ENOENT 归类成 not_found 而非一律上报 internal（2026-10-07）。
// 非操作类错误（参数缺失、语义拒绝等）仍用 fsErr。
func fsOpErr(action string, err error) error {
	if err == nil {
		return nil
	}
	return &opError{action: action, err: err}
}

type opError struct {
	action string
	err    error
}

func (e *opError) Error() string {
	if e.action == "" {
		return "fs: " + e.err.Error()
	}
	return "fs " + e.action + ": " + e.err.Error()
}

func (e *opError) Unwrap() error { return e.err }
