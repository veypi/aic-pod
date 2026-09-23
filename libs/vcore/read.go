package vcore

import (
	"bufio"
	"context"
	"fmt"
	"strings"
	"unicode/utf8"
)

// readTailLines 是 read 越界回退窗口的行数（§4.2）：offset 越过文件头/尾时
// 不再返回 state=error，而是就近回退为有界窗口并把说明写入 attrs.note——
// 弱模型「越界-报错-纠偏」一轮的往返成本高于宽容回退。
const readTailLines = 100

// fsRead 实现 read（§4.2）：offset/limit 1 基，与 Content 行号同一编号空间。
// 数值参数不报错：offset<1 回退文件头窗口、offset>total 回退文件尾窗口、
// limit<1 用缺省 1000；回退原因与实际窗口写 attrs.note。
func fsRead(ctx context.Context, env *Env, p *fsParams) (*Result, error) {
	if p.Path == "" {
		return nil, fsErr("read", "path is required")
	}
	offset, limit := 1, 1000
	if p.Offset != nil {
		offset = *p.Offset
	}
	limitNote := ""
	if p.Limit != nil {
		switch {
		case *p.Limit < 1:
			limitNote = fmt.Sprintf("limit %d must be >= 1; used the default limit (1000)", *p.Limit)
		case *p.Limit < 1000:
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
	if err := env.CheckPolicy("fs read", abs, false); err != nil {
		return nil, err
	}

	info, statErr := env.VFS.Stat(abs)
	if statErr == nil && !info.IsDir() && info.Size() > streamThreshold {
		return fsReadLarge(env, abs, offset, limit, limitNote)
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
	if total == 0 {
		return emptyReadResult(abs, offset, limitNote), nil
	}
	start, end, fb := readWindow(offset, limit, total)

	var b strings.Builder
	for i := start - 1; i < end; i++ {
		fmt.Fprintf(&b, "%d\t%s\n", i+1, lines[i])
	}
	body := b.String()
	// 128KB 内容上限先于 limit 触发：只保留完整行，rows/range 同步收紧（§2.5）
	if cut, wasCut := truncateContent(body, MaxContentBytes); wasCut {
		body = cut
		end = start - 1 + strings.Count(body, "\n")
	}
	truncated := end < total

	r := newResult("read", abs)
	r.Content = body
	r.Attrs["mime"] = "text/plain"
	r.set("total_lines", total)
	r.set("rows", end-(start-1))
	r.set("range", fmt.Sprintf("%d-%d", start, end))
	r.set("truncated", truncated)
	if truncated {
		r.set("hint", readHint(total, start, end))
	}
	if n := readNote(fb, offset, total, start, end, limitNote); n != "" {
		r.set("note", n)
	}
	return r, nil
}

// emptyReadResult 空文件（0 行）read：空正文 + 说明（§4.2 回退语义，不再报错）。
func emptyReadResult(abs string, offset int, limitNote string) *Result {
	r := newResult("read", abs)
	r.Attrs["mime"] = "text/plain"
	r.set("total_lines", 0)
	r.set("rows", 0)
	r.set("range", "0-0")
	r.set("truncated", false)
	var parts []string
	switch {
	case offset < 1:
		parts = append(parts, fmt.Sprintf("offset %d must be >= 1", offset))
	case offset > 1:
		parts = append(parts, fmt.Sprintf("offset %d exceeds 0 lines", offset))
	}
	parts = append(parts, "file is empty (0 lines)")
	if limitNote != "" {
		parts = append(parts, limitNote)
	}
	r.set("note", strings.Join(parts, "; "))
	return r
}

// 回退类别（readWindow 返回；决定 note 文案，§4.2）。
const (
	fbNone = iota // 无回退（常规截断由 hint 表达）
	fbHead        // offset<1：文件头窗口
	fbTail        // offset>total：文件尾窗口
)

// readWindow 计算实际读取窗口 [start, end]（1 基闭区间）与回退类别（§4.2）。
// 数值越界不报错，就近回退为有界窗口；调用方保证 total ≥ 1。
func readWindow(offset, limit, total int) (start, end, fb int) {
	if offset < 1 {
		end = total
		if end > readTailLines {
			end = readTailLines
		}
		return 1, end, fbHead
	}
	if offset > total {
		start = total - readTailLines + 1
		if start < 1 {
			start = 1
		}
		return start, total, fbTail
	}
	end = offset - 1 + limit
	if end > total {
		end = total
	}
	return offset, end, fbNone
}

// readNote 组装回退说明（§4.2）：回退类别 + limit 非法，分号连接；无说明返回 ""。
// 窗口区间取调用方定稿值（预算收刀后），与 attrs.range 恒一致。
func readNote(fb, offset, total, start, end int, limitNote string) string {
	var parts []string
	switch fb {
	case fbHead:
		parts = append(parts, fmt.Sprintf("offset %d must be >= 1; returned lines %d-%d", offset, start, end))
	case fbTail:
		parts = append(parts, fmt.Sprintf("offset %d exceeds %d lines; returned lines %d-%d", offset, total, start, end))
	}
	if limitNote != "" {
		parts = append(parts, limitNote)
	}
	return strings.Join(parts, "; ")
}

// readHint 截断时的翻页提示（放 attrs 而非 content，避免被当作正文，§4.2）。
func readHint(total, offset, end int) string {
	return fmt.Sprintf("file has %d lines; this call returned %d-%d; pass offset=%d to continue reading", total, offset, end, end+1)
}

// fsReadLarge 流式读取大文件（>8MB）：单次按行扫描，总行数精确统计，
// 仅缓冲窗口内且在 128KB 预算内的行（§4.2，三端一致）。
// offset<1 先按文件头窗口回退；offset>total 在总行数已知后重扫取文件尾窗口。
func fsReadLarge(env *Env, abs string, offset, limit int, limitNote string) (*Result, error) {
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

	start, scanLimit := offset, limit
	if start < 1 {
		start, scanLimit = 1, readTailLines // 文件头窗口回退（offset<1）
	}
	var b strings.Builder
	total, kept := 0, 0
	for {
		line, err := r.ReadString('\n')
		if len(line) > 0 {
			total++
			line = strings.TrimSuffix(line, "\n")
			if total >= start && kept < scanLimit {
				row := fmt.Sprintf("%d\t%s\n", total, line)
				// 128KB 预算：首行超限截断写入（与 fsRead 整读路径
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
	if total == 0 {
		return emptyReadResult(abs, offset, limitNote), nil
	}

	fb := fbNone
	switch {
	case offset < 1:
		fb = fbHead
	case offset > total:
		// 文件尾窗口回退：总行数已知后重扫（仅此分支付出二次扫描，
		// 换取免常驻尾环缓冲，§4.2）
		fb = fbTail
		start = total - readTailLines + 1
		if start < 1 {
			start = 1
		}
		body, kept2, err := readTailWindow(env, abs, start, readTailLines)
		if err != nil {
			return nil, fsErr("read", "%s", err)
		}
		b.Reset()
		b.WriteString(body)
		kept = kept2
	}

	end := start - 1 + kept
	truncated := end < total
	r2 := newResult("read", abs)
	r2.Content = b.String()
	r2.Attrs["mime"] = "text/plain"
	r2.set("total_lines", total)
	r2.set("rows", kept)
	r2.set("range", fmt.Sprintf("%d-%d", start, end))
	r2.set("truncated", truncated)
	if truncated {
		r2.set("hint", readHint(total, start, end))
	}
	if n := readNote(fb, offset, total, start, end, limitNote); n != "" {
		r2.set("note", n)
	}
	return r2, nil
}

// readTailWindow 重扫文件收集 [start, start+limit) 窗口的行（文件尾回退专用；
// 行号与预算规则与主流式扫描一致，§4.2）。
func readTailWindow(env *Env, abs string, start, limit int) (string, int, error) {
	f, err := env.VFS.Open(abs)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64<<10)
	var b strings.Builder
	total, kept := 0, 0
	for kept < limit {
		line, err := r.ReadString('\n')
		if len(line) > 0 {
			total++
			if total >= start {
				row := fmt.Sprintf("%d\t%s\n", total, strings.TrimSuffix(line, "\n"))
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
	return b.String(), kept, nil
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
