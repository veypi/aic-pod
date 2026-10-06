package fsx

// edit_diagnose.go — edit 失败诊断（P0）：ambiguous 给命中行号列表；
// not-found 给最近似候选（行号 + 该处真实文本 + 相似度）。
//
// 归一化管线移植自 vsh contrib/codingtools（edit_diff.go），仅作诊断用途，
// 不参与编辑应用（P3 fuzzy 应用复用本管线）。三端对齐：page_fs.js 为 JS
// 等价实现（NFKC 用原生 String.prototype.normalize）。

import (
	"math"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// editNearest 是 not-found 失败的近似定位诊断。
type editNearest struct {
	Line int `json:"line"`
	// Kind = "normalized"（归一化后精确命中：空白/标点/Unicode 漂移类）
	//      | "similar"（行级相似度候选）。
	Kind       string  `json:"kind"`
	Similarity float64 `json:"similarity,omitempty"` // 仅 kind=similar
	Text       string  `json:"text"`                 // 该行实际文本 ±2 行（截断 300 字节）
}

const (
	// diagnoseMaxFile 是近似诊断的文件大小上限（性能保护，超出跳过）。
	diagnoseMaxFile = 1 << 20
	// diagnoseMinAnchor 是近似诊断的最小锚长度（归一化去空白后字节数；
	// 短锚近似无信息量且误报多）。
	diagnoseMinAnchor = 8
	// diagnoseSimilarityFloor 是行级相似度报告阈值（bigram Dice）。
	diagnoseSimilarityFloor = 0.6
	// snippetMaxBytes 是诊断文本片段上限。
	snippetMaxBytes = 300
)

// editDiagnoser 缓存一次 edit 调用内失败诊断的派生数据（行索引即时构建，
// 归一化管线懒构建——只在出现 not-found 时才付归一化成本）。
// 每次成功应用编辑后内容变化，诊断器须重建（NewEditDiagnoser）。
type editDiagnoser struct {
	content string
	offs    []int    // 行首字节偏移（1-based 行号 → offs[line-1]）
	lines   []string // 按 \n 切分的原始行
	norm    string   // 懒：归一化全文
	segs    []fuzzySegment
	normed  bool
	normLn  []string // 懒：逐行归一化
	lined   bool
}

func newEditDiagnoser(content string) *editDiagnoser {
	offs := []int{0}
	for i := 0; i < len(content); i++ {
		if content[i] == '\n' {
			offs = append(offs, i+1)
		}
	}
	return &editDiagnoser{
		content: content,
		offs:    offs,
		lines:   strings.Split(content, "\n"),
	}
}

// lineOf 返回字节偏移对应的 1-based 行号。
func (d *editDiagnoser) lineOf(off int) int {
	lo, hi := 0, len(d.offs)-1
	for lo < hi {
		mid := (lo + hi + 1) >> 1
		if d.offs[mid] <= off {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo + 1
}

// matchLines 返回 old 全部出现位置的 1-based 行号（含重叠出现，供 ambiguous 诊断）。
func (d *editDiagnoser) matchLines(old string) []int {
	var lines []int
	for start := 0; start+len(old) <= len(d.content); {
		idx := strings.Index(d.content[start:], old)
		if idx < 0 {
			break
		}
		lines = append(lines, d.lineOf(start+idx))
		start += idx + 1
	}
	return lines
}

// nearest 定位 not-found 失败的最近似候选；无合格候选返回 nil。
func (d *editDiagnoser) nearest(oldText string) *editNearest {
	if len(d.content) > diagnoseMaxFile {
		return nil
	}
	if !d.normed {
		d.norm, d.segs = normalizeForFuzzyMatchWithSegments(d.content)
		d.normed = true
	}
	normOld := normalizeForFuzzyMatch(oldText)
	if len(strings.TrimSpace(normOld)) < diagnoseMinAnchor {
		return nil
	}
	// 阶段 1：归一化子串命中——锚文本在归一化后存在，说明是空白/标点/
	// Unicode 漂移；映射回原文行号，给出真实文本（AI 照抄即可修复）。
	if idx := strings.Index(d.norm, normOld); idx >= 0 {
		if s, _, ok := mapFuzzyRangeToOriginal(d.segs, idx, idx+len(normOld)); ok {
			line := d.lineOf(s)
			return &editNearest{Line: line, Kind: "normalized", Text: d.snippet(line)}
		}
	}
	// 阶段 2：行级相似度（bigram Dice）——锚行本身有内容偏差时的近似定位。
	if !d.lined {
		d.normLn = make([]string, len(d.lines))
		for i, ln := range d.lines {
			d.normLn[i] = normalizeForFuzzyMatch(ln)
		}
		d.lined = true
	}
	anchors := anchorLinesOf(normOld)
	if len(anchors) == 0 {
		return nil
	}
	best, bestLine := 0.0, 0
	for i, nl := range d.normLn {
		if nl == "" {
			continue
		}
		for _, a := range anchors {
			if s := bigramDice(a, nl); s > best {
				best, bestLine = s, i+1
			}
		}
	}
	if best >= diagnoseSimilarityFloor {
		return &editNearest{
			Line:       bestLine,
			Kind:       "similar",
			Similarity: math.Round(best*100) / 100,
			Text:       d.snippet(bestLine),
		}
	}
	return nil
}

// snippet 取 line 前后各 2 行原始文本，超 snippetMaxBytes 按 rune 边界截断。
func (d *editDiagnoser) snippet(line int) string {
	start := max(line-2, 1)
	end := min(line+2, len(d.lines))
	s := strings.Join(d.lines[start-1:end], "\n")
	if len(s) > snippetMaxBytes {
		cut := snippetMaxBytes
		for cut > 0 && !utf8.ValidString(s[:cut]) {
			cut--
		}
		s = s[:cut] + "…"
	}
	return s
}

// anchorLinesOf 从归一化锚文本取代表行：第一非空行 + 最长行（去重）。
func anchorLinesOf(normOld string) []string {
	var first, longest string
	for _, ln := range strings.Split(normOld, "\n") {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		if first == "" {
			first = ln
		}
		if len(ln) > len(longest) {
			longest = ln
		}
	}
	if first == "" {
		return nil
	}
	if longest == "" || longest == first {
		return []string{first}
	}
	return []string{first, longest}
}

// bigramDice 计算两字符串的 rune 二元组 Dice 系数（0-1）。
func bigramDice(a, b string) float64 {
	if a == "" || b == "" {
		return 0
	}
	ra, rb := []rune(a), []rune(b)
	if len(ra) < 2 || len(rb) < 2 {
		if a == b {
			return 1
		}
		return 0
	}
	counts := make(map[[2]rune]int, len(ra)-1)
	for i := 0; i+1 < len(ra); i++ {
		counts[[2]rune{ra[i], ra[i+1]}]++
	}
	overlap := 0
	for i := 0; i+1 < len(rb); i++ {
		g := [2]rune{rb[i], rb[i+1]}
		if counts[g] > 0 {
			overlap++
			counts[g]--
		}
	}
	return 2 * float64(overlap) / float64(len(ra)+len(rb)-2)
}

// ---- 归一化管线（移植自 codingtools edit_diff.go；CRLF/CR 归一为 LF） ----

type fuzzySegment struct {
	output    string
	origStart int
	origEnd   int
	outStart  int
	outEnd    int
}

// fuzzyMatchReplacer 智能标点/空白归一表（与 codingtools 一致）。
// 全部用 \u 转义书写：特殊空白/标点字符经文件传输易被静默归一。
var fuzzyMatchReplacer = strings.NewReplacer(
	"\u2018", "'",
	"\u2019", "'",
	"\u201A", "'",
	"\u201B", "'",
	"\u201C", "\"",
	"\u201D", "\"",
	"\u201E", "\"",
	"\u201F", "\"",
	"\u2010", "-",
	"\u2011", "-",
	"\u2012", "-",
	"\u2013", "-",
	"\u2014", "-",
	"\u2015", "-",
	"\u2212", "-",
	"\u00A0", " ",
	"\u2002", " ",
	"\u2003", " ",
	"\u2004", " ",
	"\u2005", " ",
	"\u2006", " ",
	"\u2007", " ",
	"\u2008", " ",
	"\u2009", " ",
	"\u200A", " ",
	"\u202F", " ",
	"\u205F", " ",
	"\u3000", " ",
)

func normalizeForFuzzyMatch(text string) string {
	normalized, _ := normalizeForFuzzyMatchWithSegments(text)
	return normalized
}

// normalizeForFuzzyMatchWithSegments 归一化文本并保留回原偏移的映射段：
// 逐行 NFKC + 标点/空白替换 + 行尾空白 trim；换行统一输出 "\n"
// （CRLF/CR 输入的原始两/单字节由原段覆盖）。
func normalizeForFuzzyMatchWithSegments(text string) (string, []fuzzySegment) {
	segments := make([]fuzzySegment, 0, len(text)/16)
	var builder strings.Builder

	lineStart := 0
	flushLine := func(lineEnd int) {
		line := text[lineStart:lineEnd]
		lineSegments := normalizeLineForFuzzyMatchSegments(line, lineStart)
		for _, segment := range lineSegments {
			segment.outStart = builder.Len()
			builder.WriteString(segment.output)
			segment.outEnd = builder.Len()
			segments = append(segments, segment)
		}
	}
	i := 0
	for i < len(text) {
		if text[i] != '\r' && text[i] != '\n' {
			i++
			continue
		}
		flushLine(i)
		nlEnd := i + 1
		if text[i] == '\r' && nlEnd < len(text) && text[nlEnd] == '\n' {
			nlEnd++
		}
		newline := fuzzySegment{
			output:    "\n",
			origStart: i,
			origEnd:   nlEnd,
			outStart:  builder.Len(),
		}
		builder.WriteString("\n")
		newline.outEnd = builder.Len()
		segments = append(segments, newline)
		i = nlEnd
		lineStart = nlEnd
	}
	if lineStart < len(text) {
		flushLine(len(text))
	}
	return builder.String(), segments
}

func normalizeLineForFuzzyMatchSegments(line string, baseOffset int) []fuzzySegment {
	segments := make([]fuzzySegment, 0, len(line))
	for start := 0; start < len(line); {
		size := norm.NFKC.NextBoundaryInString(line[start:], true)
		if size <= 0 {
			size = len(line) - start
		}
		chunk := line[start : start+size]
		normalized := fuzzyMatchReplacer.Replace(norm.NFKC.String(chunk))
		if normalized != "" {
			segments = append(segments, fuzzySegment{
				output:    normalized,
				origStart: baseOffset + start,
				origEnd:   baseOffset + start + size,
			})
		}
		start += size
	}
	return trimTrailingFuzzySegments(segments)
}

func trimTrailingFuzzySegments(segments []fuzzySegment) []fuzzySegment {
	for len(segments) > 0 {
		last := len(segments) - 1
		trimmed := strings.TrimRight(segments[last].output, " \t")
		if trimmed == segments[last].output {
			break
		}
		if trimmed == "" {
			segments = segments[:last]
			continue
		}
		segments[last].output = trimmed
		break
	}
	return segments
}

// mapFuzzyRangeToOriginal 把归一化文本的 [start,end) 映射回原文偏移区间。
func mapFuzzyRangeToOriginal(segments []fuzzySegment, start, end int) (int, int, bool) {
	if start < 0 || end <= start {
		return 0, 0, false
	}
	var startSegment, endSegment *fuzzySegment
	for i := range segments {
		segment := &segments[i]
		if startSegment == nil && start >= segment.outStart && start < segment.outEnd {
			startSegment = segment
		}
		if end > segment.outStart && end <= segment.outEnd {
			endSegment = segment
			break
		}
	}
	if startSegment == nil || endSegment == nil {
		return 0, 0, false
	}
	return startSegment.origStart, endSegment.origEnd, true
}
