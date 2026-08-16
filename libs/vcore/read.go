package vcore

import (
	"bufio"
	"context"
	"fmt"
	"strings"
	"unicode/utf8"
)

// fsRead 实现 read（§4.2）：offset/limit 1 基，与 Content 行号同一编号空间。
func fsRead(ctx context.Context, env *Env, p *fsParams) (*Result, error) {
	if p.Path == "" {
		return nil, fsErr("read", "path is required")
	}
	offset, limit := 1, 1000
	if p.Offset != nil {
		if *p.Offset < 1 {
			return nil, fsErr("read", "offset must be >= 1, got %d", *p.Offset)
		}
		offset = *p.Offset
	}
	if p.Limit != nil {
		if *p.Limit < 1 {
			return nil, fsErr("read", "limit must be >= 1, got %d", *p.Limit)
		}
		if *p.Limit < 1000 {
			limit = *p.Limit
		}
	}
	abs, err := env.Resolve(p.Path)
	if err != nil {
		return nil, fsErr("read", "%s", err)
	}
	if err := env.CheckPath("fs", abs); err != nil {
		return nil, err
	}

	info, statErr := env.VFS.Stat(abs)
	if statErr == nil && !info.IsDir() && info.Size() > streamThreshold {
		return fsReadLarge(env, abs, offset, limit)
	}

	data, err := env.VFS.ReadFile(abs)
	if err != nil {
		return nil, fsErr("read", "%s", err)
	}
	if !isTextContent(data) {
		return binaryResult(env, abs, data)
	}

	lines := strings.Split(string(data), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	total := len(lines)
	if offset > total {
		return nil, fsErr("read", "offset %d exceeds %d lines", offset, total)
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
	// 512KB 内容上限先于 limit 触发：只保留完整行，rows/range 同步收紧（§4.2）
	if cut, wasCut := truncateContent(body, MaxContentBytes); wasCut {
		body = cut
		end = offset - 1 + strings.Count(body, "\n")
		truncated = true
	}

	r := newResult("read", abs)
	r.Content = body
	r.Attrs["mime"] = "text/plain"
	r.set("total_lines", total)
	r.set("rows", end-(offset-1))
	r.set("range", fmt.Sprintf("%d-%d", offset, end))
	r.set("truncated", truncated)
	return r, nil
}

// fsReadLarge 流式读取大文件（>8MB）：单次按行扫描，总行数精确统计，
// 仅缓冲窗口内且在 512KB 预算内的行（§4.2，三端一致）。
func fsReadLarge(env *Env, abs string, offset, limit int) (*Result, error) {
	f, err := env.VFS.Open(abs)
	if err != nil {
		return nil, fsErr("read", "%s", err)
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64<<10)
	head, _ := r.Peek(512)
	// 截断的多字节字符尾部不计入 UTF-8 判定
	for n := 0; n < 3 && len(head) > 0 && !utf8.Valid(head); n++ {
		head = head[:len(head)-1]
	}
	if !isTextContent(head) {
		return largeBinaryResult(env, abs, head)
	}

	var b strings.Builder
	total, kept := 0, 0
	for {
		line, err := r.ReadString('\n')
		if len(line) > 0 {
			total++
			line = strings.TrimSuffix(line, "\n")
			if total >= offset && kept < limit {
				row := fmt.Sprintf("%d\t%s\n", total, line)
				// 512KB 预算：首行超限截断写入（与 fsRead 整读路径
				// truncateContent 语义一致），后续行超限跳过（§2.5）
				if b.Len()+len(row) <= MaxContentBytes {
					b.WriteString(row)
					kept++
				} else if kept == 0 {
					if cut, _ := truncateContent(row, MaxContentBytes); cut != "" {
						b.WriteString(cut)
						kept++
					}
				}
			}
		}
		if err != nil {
			break
		}
	}
	if offset > total {
		return nil, fsErr("read", "offset %d exceeds %d lines", offset, total)
	}
	end := offset - 1 + kept
	r2 := newResult("read", abs)
	r2.Content = b.String()
	r2.Attrs["mime"] = "text/plain"
	r2.set("total_lines", total)
	r2.set("rows", kept)
	r2.set("range", fmt.Sprintf("%d-%d", offset, end))
	r2.set("truncated", end < total)
	return r2, nil
}

// binaryResult 生成二进制 read 结果（§4.2）：mime + size；可展示图片按 §2.2 图片标准。
func binaryResult(env *Env, abs string, data []byte) (*Result, error) {
	mime := detectMIME(data, abs)
	r := newResult("read", abs)
	r.Attrs["mime"] = mime
	r.set("size", len(data))
	if isViewableImageMime(mime) {
		return imageResult(env, abs, data, mime)
	}
	r.Content = fmt.Sprintf("Binary file: %s (%s, %d bytes)", abs, mime, len(data))
	return r, nil
}

// largeBinaryResult 生成大二进制结果：不整读，size 取自 Stat，图片跳过尺寸探测（§4.2）。
func largeBinaryResult(env *Env, abs string, head []byte) (*Result, error) {
	info, err := env.VFS.Stat(abs)
	if err != nil {
		return nil, fsErr("read", "%s", err)
	}
	mime := detectMIME(head, abs)
	r := newResult("read", abs)
	r.Attrs["mime"] = mime
	r.set("size", info.Size())
	if isViewableImageMime(mime) {
		if env.ImageData {
			// host/page 端图片仍需整读以压缩产出 image_data（§4.2 环境能力差异）
			data, err := env.VFS.ReadFile(abs)
			if err != nil {
				return nil, fsErr("read", "%s", err)
			}
			return imageResult(env, abs, data, mime)
		}
		r.Attrs["image_path"] = abs
		r.Content = fmt.Sprintf("Image file: %s (%s, %d bytes)", abs, mime, info.Size())
		return r, nil
	}
	r.Content = fmt.Sprintf("Binary file: %s (%s, %d bytes)", abs, mime, info.Size())
	return r, nil
}
