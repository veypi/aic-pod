// FS 数据面的结构化位置与写入条件（独立于 AI 文本格式化与物理 host 路径语法）。
package protocol

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const FSContract = "fs/1"

type FSPath struct {
	RootID   string   `json:"root_id"`
	Segments []string `json:"segments"`
}

func (p FSPath) Validate(windows bool) error {
	if !ValidID(p.RootID) || p.Segments == nil || len(p.Segments) > 256 {
		return Fail("invalid_argument", "Invalid file location")
	}
	total := 0
	for _, s := range p.Segments {
		total += len(s)
		if s == "" || s == "." || s == ".." || !utf8.ValidString(s) || strings.ContainsAny(s, "/\x00") || windows && strings.ContainsAny(s, `\:`) || windows && (strings.HasSuffix(s, ".") || strings.HasSuffix(s, " ")) {
			return Fail("invalid_argument", "Invalid path segment")
		}
	}
	if total > 16384 {
		return Fail("invalid_argument", "Path exceeds limit")
	}
	return nil
}

type FSEntry struct {
	Path       FSPath `json:"path"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Size       *int64 `json:"size,omitempty"`
	ModifiedAt string `json:"modified_at,omitempty"`
	Version    string `json:"version"`
	MediaType  string `json:"media_type,omitempty"`
}
type FSCondition struct {
	Absent  bool   `json:"absent,omitempty"`
	Version string `json:"version,omitempty"`
	Any     bool   `json:"any,omitempty"`
}

func (c FSCondition) Validate() error {
	n := 0
	if c.Absent {
		n++
	}
	if c.Version != "" {
		n++
	}
	if c.Any {
		n++
	}
	if n != 1 {
		return Fail("invalid_argument", "Exactly one write condition is required")
	}
	return nil
}
func (c FSCondition) Check(version string, exists bool) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if c.Absent && exists {
		return Fail("already_exists", "Destination already exists")
	}
	if c.Version != "" && (!exists || c.Version != version) {
		f := Fail("version_conflict", "File changed since it was read")
		f.Details = map[string]any{"current_version": version}
		return f
	}
	return nil
}
func (p FSPath) String() string { return fmt.Sprintf("%s/%s", p.RootID, strings.Join(p.Segments, "/")) }

// FSResourceRef 是上传/下载字料的身份引用（owner 隔离由服务层校验）。
type FSResourceRef struct {
	ID    string `json:"id"`
	Epoch string `json:"epoch"`
	Kind  string `json:"kind"`
}

// FSMaxSafeInteger 是 JS 互通的整数上限。
const FSMaxSafeInteger = 1<<53 - 1

// FSFail 构造 FS 失败：Effect=none——FS 各失败入口保证无任何副作用
// （批次 2 错误模型收敛后保留此语义，不得机械替换为无 Effect 的 Fail）。
func FSFail(code, message string) *Fault { f := Fail(code, message); f.Effect = "none"; return f }
