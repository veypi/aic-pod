package vcore

import (
	"context"
	"fmt"
	"path"
	"strings"
)

// fsWrite 实现 write（§4.3）：content 必填；默认整文件覆写，append 为显式追加；
// 父目录不存在自动创建。
func fsWrite(ctx context.Context, env *Env, p *fsParams) (*Result, error) {
	if p.Path == "" {
		return nil, fsErr("write", "path is required")
	}
	if p.Content == nil {
		return nil, fsErr("write", "content is required")
	}
	abs, err := env.Resolve(p.Path)
	if err != nil {
		return nil, fsErr("write", "%s", err)
	}
	content := *p.Content
	if err := env.VFS.MkdirAll(path.Dir(abs)); err != nil {
		return nil, fsErr("write", "%s", err)
	}

	lines := countLines(content)
	r := newResult("write", abs)
	r.set("lines", lines)
	r.set("bytes", len(content))

	if p.Append {
		if ap, ok := env.VFS.(Appender); ok {
			err = ap.Append(abs, []byte(content))
		} else {
			// 读改写（§4.3 实现取舍：cloud/page 无追加原语，同 host 串行化挡并发交叉）
			var old []byte
			old, err = env.VFS.ReadFile(abs)
			if err != nil {
				old = nil // 文件不存在则创建
			}
			err = env.VFS.WriteFile(abs, append(old, content...))
		}
		if err != nil {
			return nil, fsErr("write", "%s", err)
		}
		r.Content = fmt.Sprintf("appended to %s (+%d lines, +%d bytes)", abs, lines, len(content))
		r.Attrs["mode"] = "append"
		return r, nil
	}

	if err := env.VFS.WriteFile(abs, []byte(content)); err != nil {
		return nil, fsErr("write", "%s", err)
	}
	r.Content = fmt.Sprintf("wrote file: %s (%d lines, %d bytes)", abs, lines, len(content))
	r.Attrs["mode"] = "overwrite"
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

// fsEdit 实现 edit（§4.4）：edits 数组一次多处替换；每个 oldText 在文件中
// 必须唯一匹配；各 edit 基于原始文件匹配、互不重叠（不提供 replaceAll）。
func fsEdit(ctx context.Context, env *Env, p *fsParams) (*Result, error) {
	if p.Path == "" {
		return nil, fsErr("edit", "path is required")
	}
	if len(p.Edits) == 0 {
		return nil, fsErr("edit", "edits is required")
	}
	for _, e := range p.Edits {
		if e.OldText == "" {
			return nil, fsErr("edit", "oldText is required")
		}
		if e.NewText == e.OldText {
			return nil, fsErr("edit", "newText must be different from oldText")
		}
	}
	abs, err := env.Resolve(p.Path)
	if err != nil {
		return nil, fsErr("edit", "%s", err)
	}
	data, err := env.VFS.ReadFile(abs)
	if err != nil {
		return nil, fsErr("edit", "%s", err)
	}
	if !isTextContent(data) {
		return nil, fsErr("edit", "%s is not a text file", abs)
	}
	content := string(data)

	// 基于原始文件定位每个 edit 的唯一匹配区间
	type span struct{ start, end, idx int }
	spans := make([]span, 0, len(p.Edits))
	for i, e := range p.Edits {
		first := strings.Index(content, e.OldText)
		if first < 0 {
			return nil, fsErr("edit", "oldText not found in file")
		}
		if strings.Index(content[first+len(e.OldText):], e.OldText) >= 0 {
			n := strings.Count(content, e.OldText)
			return nil, fsErr("edit",
				"oldText matches %d locations; provide more surrounding context to make it unique", n)
		}
		spans = append(spans, span{first, first + len(e.OldText), i})
	}
	// 重叠/嵌套校验：报错引导合并为单个 edit
	for a := 0; a < len(spans); a++ {
		for b := a + 1; b < len(spans); b++ {
			if spans[a].start < spans[b].end && spans[b].start < spans[a].end {
				return nil, fsErr("edit",
					"edits overlap or nest; merge them into a single edit")
			}
		}
	}
	// 自右向左替换，区间基于原始文件
	for i := 0; i < len(spans); i++ {
		for j := i + 1; j < len(spans); j++ {
			if spans[j].start > spans[i].start {
				spans[i], spans[j] = spans[j], spans[i]
			}
		}
	}
	for _, s := range spans {
		content = content[:s.start] + p.Edits[s.idx].NewText + content[s.end:]
	}
	if err := env.VFS.WriteFile(abs, []byte(content)); err != nil {
		return nil, fsErr("edit", "%s", err)
	}
	r := newResult("edit", abs)
	r.Content = fmt.Sprintf("updated file: %s (%d edits)", abs, len(p.Edits))
	r.set("edits", len(p.Edits))
	return r, nil
}
