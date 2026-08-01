package vcore

import (
	"context"
	"fmt"
	"path"
	"strings"
)

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

// fsEdit 实现 edit（§4.4）：edits 数组逐个顺序应用——每个 edit 基于前一个
// 应用后的当前内容匹配，oldText 必须唯一；单个 edit 失败（找不到/多匹配/
// 参数非法）不阻塞其余 edit——**部分成功语义**：成功的保留，失败的按
// `edit[i]: 原因` 在结果中报告（不提供 replaceAll）。全部失败时整组报错、
// 不写文件。
func fsEdit(ctx context.Context, env *Env, p *fsParams) (*Result, error) {
	if p.Path == "" {
		return nil, fsErr("edit", "path is required")
	}
	if len(p.Edits) == 0 {
		return nil, fsErr("edit", "edits is required")
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

	// 逐个顺序应用（§4.4）：后一个 edit 匹配的是前一个应用后的内容。
	// 失败条目记录序号与原因，不阻塞其余 edit（部分成功语义）。
	applied := 0
	failed := make([]string, 0)
	for i, e := range p.Edits {
		idx := fmt.Sprintf("edit[%d]", i+1)
		if e.OldText == "" {
			failed = append(failed, idx+": oldText is required")
			continue
		}
		if e.NewText == e.OldText {
			failed = append(failed, idx+": newText must be different from oldText")
			continue
		}
		first := strings.Index(content, e.OldText)
		if first < 0 {
			failed = append(failed, idx+": oldText not found in file")
			continue
		}
		if n := strings.Count(content, e.OldText); n > 1 {
			failed = append(failed, fmt.Sprintf(
				"%s: oldText matches %d locations; provide more surrounding context to make it unique", idx, n))
			continue
		}
		content = content[:first] + e.NewText + content[first+len(e.OldText):]
		applied++
	}
	if applied == 0 {
		// 单 edit 失败直接报原因；多 edit 全失败汇总报（都不写文件）
		if len(failed) == 1 {
			return nil, fsErr("edit", "%s", failed[0])
		}
		return nil, fsErr("edit", "no edits applied: %s", strings.Join(failed, "; "))
	}
	if err := env.VFS.WriteFile(abs, []byte(content)); err != nil {
		return nil, fsErr("edit", "%s", err)
	}
	r := newResult("edit", abs)
	if len(failed) == 0 {
		r.Content = fmt.Sprintf("updated file: %s (%d edits)", abs, len(p.Edits))
	} else {
		r.Content = fmt.Sprintf("updated file: %s (%d/%d edits applied; failed: %s)",
			abs, applied, len(p.Edits), strings.Join(failed, "; "))
		r.set("edits_failed", len(failed))
	}
	r.set("edits", applied)
	return r, nil
}
