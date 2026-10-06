package fsx

import (
	"context"
	"encoding/json"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// writeOutcome / editOutcome 是 write/edit 的结构化返回（v3 约定：
// 成功极简、失败详尽；path 等请求侧已知信息不回显；Content 为 JSON
// 文档——parse it, do not regex）。
type writeOutcome struct {
	OK    bool   `json:"ok"`
	Lines int    `json:"lines"`
	Bytes int    `json:"bytes"`
	V     string `json:"v"`
}

type editOutcome struct {
	OK      bool          `json:"ok"`
	Applied int           `json:"applied"`
	V       string        `json:"v,omitempty"` // 写后内容版本（无落盘则无）
	Errors  []editFailure `json:"errors,omitempty"`
}

// editFailure 是单条 edit 的失败诊断：成功集合 = 全集减去 errors 的 i。
type editFailure struct {
	I      int    `json:"i"` // 1-based 条目序号
	Reason string `json:"reason"`
	// ambiguous：全部命中行号
	Matches []int `json:"matches,omitempty"`
	// not-found：最近似候选（归一化命中或行级相似）
	Nearest *editNearest `json:"nearest,omitempty"`
	// 可选修复提示（如双重编码 hint）
	Hint string `json:"hint,omitempty"`
}

func marshalOutcome(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return `{"ok":false,"reason":"internal-marshal-error"}`
	}
	return string(data)
}

// fsWrite 实现 write（§4.3）：content 必填，整文件覆写；父目录不存在自动创建。
func fsWrite(ctx context.Context, env *Env, p *fsParams) (*Result, error) {
	if p.Path == "" {
		return nil, fsErr("write", "path is required")
	}
	if p.Content == nil {
		return nil, fsErr("write", "content is required")
	}
	abs, err := env.Resolve(p.Path)
	if err != nil {
		return nil, fsOpErr("write", err)
	}
	if err := env.CheckPath("fs", abs); err != nil {
		return nil, err
	}
	if err := env.CheckPolicy("fs write", abs, true); err != nil {
		return nil, err
	}
	content := *p.Content
	if err := env.FS.MkdirAll(path.Dir(abs), 0o755); err != nil {
		return nil, fsOpErr("write", err)
	}
	if err := env.FS.WriteFile(abs, []byte(content), 0o644); err != nil {
		return nil, fsOpErr("write", err)
	}
	lines := countLines(content)
	r := newResult("write", "")
	r.Content = marshalOutcome(writeOutcome{OK: true, Lines: lines, Bytes: len(content), V: contentVersion(content)})
	r.set("ok", true)
	r.set("lines", lines)
	return r, nil
}

// countLines 统计行数：'\n' 数量 +（末尾无换行符 ? 1 : 0）；空内容为 0 行（§4.3）。
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

// unicodeEscapeRe 匹配字面 \uXXXX 序列（LLM 双重 JSON 编码的典型残留：
// 模型意图发出 JSON 转义 \u003c 表示 '<'，但反斜杠又被转义一次，工具实际
// 收到 6 个字面字符 \u003c，与文件中的 '<' 无法匹配）。
var unicodeEscapeRe = regexp.MustCompile(`\\u[0-9a-fA-F]{4}`)

// doubleEncodingHint 检测上述双重编码：oldText 含字面 \uXXXX 转义且其 Unicode
// 解码形式恰好在文件中存在时，返回可操作提示（否则空串）。
// 仅处理 BMP 单码元；代理对无法正确还原时解码结果自然不匹配、不产生误报。
func doubleEncodingHint(content, oldText string) string {
	if !strings.Contains(oldText, `\u`) {
		return ""
	}
	decoded := unicodeEscapeRe.ReplaceAllStringFunc(oldText, func(m string) string {
		n, err := strconv.ParseUint(m[2:], 16, 32)
		if err != nil {
			return m
		}
		return string(rune(n))
	})
	if decoded == oldText || !strings.Contains(content, decoded) {
		return ""
	}
	return `oldText contains literal "\u003c"-style escapes from double JSON encoding; decoded form matches the file, resend with actual characters`
}

// fsEdit 实现 edit（§4.4，v3 结构化返回）：edits 数组逐个顺序应用——每个
// edit 基于前一个应用后的当前内容匹配，oldText 必须唯一（部分成功语义：
// 成功的保留并落盘，失败的进 errors 附诊断）。edit 级失败（not-found /
// ambiguous / 参数非法）不走 error 通道——全部失败也返回 {"ok":false}
// 且不写盘；error 仅操作性失败（文件不存在/非文本/权限/JSON 非法）。
func fsEdit(ctx context.Context, env *Env, p *fsParams) (*Result, error) {
	if p.Path == "" {
		return nil, fsErr("edit", "path is required")
	}
	if len(p.Edits) == 0 {
		return nil, fsErr("edit", "edits is required")
	}
	abs, err := env.Resolve(p.Path)
	if err != nil {
		return nil, fsOpErr("edit", err)
	}
	if err := env.CheckPath("fs", abs); err != nil {
		return nil, err
	}
	if err := env.CheckPolicy("fs edit", abs, true); err != nil {
		return nil, err
	}
	data, err := env.FS.ReadFile(abs)
	if err != nil {
		return nil, fsOpErr("edit", err)
	}
	if !isTextContent(data) {
		return nil, fsErr("edit", "%s is not a text file", abs)
	}
	content := string(data)

	// 逐个顺序应用（§4.4）：后一个 edit 匹配的是前一个应用后的内容。
	// 失败条目进 failures（附诊断），不阻塞其余 edit。
	dg := newEditDiagnoser(content)
	applied := 0
	var failures []editFailure
	for i, e := range p.Edits {
		n := i + 1
		if e.OldText == "" {
			failures = append(failures, editFailure{I: n, Reason: "empty-oldText"})
			continue
		}
		if e.NewText == e.OldText {
			failures = append(failures, editFailure{I: n, Reason: "identical-old-new"})
			continue
		}
		first := strings.Index(content, e.OldText)
		if first < 0 {
			f := editFailure{I: n, Reason: "not-found"}
			if nx := dg.nearest(e.OldText); nx != nil {
				f.Nearest = nx
			}
			if h := doubleEncodingHint(content, e.OldText); h != "" {
				f.Hint = h
			}
			failures = append(failures, f)
			continue
		}
		if strings.Count(content, e.OldText) > 1 {
			failures = append(failures, editFailure{I: n, Reason: "ambiguous", Matches: dg.matchLines(e.OldText)})
			continue
		}
		content = content[:first] + e.NewText + content[first+len(e.OldText):]
		applied++
		// 内容已变：诊断器重建（派生数据作废；归一化管线懒构建，重建廉价）。
		dg = newEditDiagnoser(content)
	}

	r := newResult("edit", "")
	if applied == 0 {
		// 全失败：不落盘，返回 ok:false + 全量诊断。
		r.Content = marshalOutcome(editOutcome{OK: false, Applied: 0, Errors: failures})
		r.set("ok", false)
		r.set("edits", 0)
		r.set("edits_failed", len(failures))
		return r, nil
	}
	if err := env.FS.WriteFile(abs, []byte(content), 0o644); err != nil {
		return nil, fsOpErr("edit", err)
	}
	out := editOutcome{OK: len(failures) == 0, Applied: applied, V: contentVersion(content), Errors: failures}
	r.Content = marshalOutcome(out)
	r.set("ok", out.OK)
	r.set("edits", applied)
	if len(failures) > 0 {
		r.set("edits_failed", len(failures))
	}
	return r, nil
}
