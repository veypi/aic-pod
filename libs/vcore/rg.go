package vcore

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// fsRg 实现 rg（§4.6）：内容搜索与文件列举。JSON 参数：
//
//	{pattern, path?, glob?, all?, limit?, context?}  内容搜索
//	{path?, glob?, all?, limit?}                     pattern 缺省 = 递归列出文件
//
// 输出为结构化 JSON（与 web_search/ls 一致，前端免解析）：
//   - 内容搜索 {"files":[{"path":P,"matches":[{"line":N,"text":T}|{"line":N,"text":T,"ctx":true}]}],
//     "note"?}——ctx 行 = context 上下文行（GNU grep -C 惯例：上下文区相邻或
//     重叠时合并，无未选行不打分隔；结构化后无 -- 分隔符，行序即上下文序）；
//     note 仅在跳过 minified 文件时出现（"N minified files skipped, use all=true to include"）；
//   - 列举模式（pattern 缺省）{"files":[P1,P2,...]}；
//   - 无匹配/无文件恒为 {"files":[]}（+可选 note）。
// 行尾 \r 剥除；同文件命中按行号升序排列。
//
// smart case（ripgrep 惯例）：pattern 不含大写字母 → 大小写不敏感；
// 含大写 → 敏感。词边界直接在 pattern 里写 \b。
//
// 对齐真实 rg：默认跳过隐藏文件与隐藏目录（点开头），all 收录；
// 平台行为（文档明示）：全局输出行数上限（默认 50，limit 可调至 200；
// 命中与上下文行同池计数）+ truncated 标记、128KB 输出预算、skipDirs
// 与点目录跳过、二进制文件跳过。
// glob 按文件名匹配（basename，* 任意序列 / ? 单字符，完整匹配），
// 多 include glob 为 OR；! 前缀 = 排除 glob；不支持 ** 跨目录。

// rgUnsupportedPatterns 是 Rust regex 同族不支持的特性清单（lookaround/backreference），
// 与 JS 端校验清单一致：命中即显式报错并引导 shell 逃生舱。
var rgUnsupportedPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\(\?<?[=!]`), // (?= (?! (?<= (?<!
	regexp.MustCompile(`\\[1-9]`),    // \1..\9 backreference
}

const rgUnsupportedHint = `pattern is not supported on this environment (restricted: no lookaround/backreference), use bash -c "grep -P ..." on a physical host`

// rgDefaultLimit 是默认全局输出行数平台上限（命中+上下文行同池）。
const rgDefaultLimit = 50

// rgMaxLimit 是 limit 参数上限（防上下文放大撑爆预算：200 行 × 单行 4KB
// 上限的理论极值仍受 128KB 字节预算收敛）。
const rgMaxLimit = 200

// rgMaxContext 是 context 参数上限（等价 grep -C 的 N）。
const rgMaxContext = 10

// rgMaxLineBytes 是匹配行内容的单行字节上限（§2.5：minified/单行超长文件
// 命中时防止单行输出撑爆总预算；超限 rune 安全截断并追加标记）。
const rgMaxLineBytes = 4 << 10 // 4KB

// clipRgText 按字节截断超长匹配行内容（rune 边界收刀），超限追加标记。
func clipRgText(text string) string {
	if len(text) <= rgMaxLineBytes {
		return text
	}
	cut := rgMaxLineBytes
	for cut > 0 && !utf8.ValidString(text[:cut]) {
		cut--
	}
	return text[:cut] + "...[truncated]"
}

// minified 判定（§4.6）：三级判定。①命名约定快路径：*.min.js/*.min.css/*.min.mjs
// 后缀即压缩语义（零 I/O，构建工具通用输出名）；②采样快路径：文件 ≥
// minifiedMinBytes 且采样前 minifiedHeadBytes 内换行数 < minifiedMinLines 即判定为
// 压缩/单行文件（minified bundle、vendored 压缩库、单行 JSON）——命中行对 AI
// 无意义（4KB 截断后仍是乱码），默认跳过（all 收录，显式单文件路径不跳过）；
// ③可疑区间兜底：头部含长 license 注释等使采样内换行 ≥ 32 的压缩库（如
// echarts.min.js：1MB/45 行，前 64KB 内 35 个换行）且采样换行 <
// minifiedSuspectLines 时，改用全文件平均行长判定——size/行数 >
// minifiedAvgLineBytes 仍判 minified。采样换行 ≥ minifiedSuspectLines 必为正常
// 格式化文件（真实代码分布：正常文件 64KB 采样换行 ≥ 1250，压缩文件 ≤ 34），
// 直接非 minified，不做全文件统计（避免正常文件多一遍全读）。
const (
	minifiedHeadBytes    = 64 << 10
	minifiedMinLines     = 32
	minifiedMinBytes     = 32 << 10
	minifiedSuspectLines = 256     // 可疑区间上界：≥ 此值必为正常文件
	minifiedAvgLineBytes = 1 << 10 // 1KB/行：正常格式化代码平均行长远低于此
)

// minifiedNameSuffixes 是压缩产物命名约定后缀（构建工具通用输出名）。
var minifiedNameSuffixes = []string{".min.js", ".min.css", ".min.mjs"}

// isMinifiedName 按命名约定判定压缩产物（后缀匹配，不依赖路径分隔符——
// Windows 盘符路径同样命中）。
func isMinifiedName(p string) bool {
	for _, s := range minifiedNameSuffixes {
		if strings.HasSuffix(p, s) {
			return true
		}
	}
	return false
}

// isMinified 判定文件是否为 minified/单行超长：三级判定（§4.6）。
// ①命名约定（*.min.js 等，零 I/O）；②采样快路径：前 minifiedHeadBytes 字节内
// 换行 < minifiedMinLines；③可疑区间兜底（采样换行 < minifiedSuspectLines 时）：
// 全文件平均行长（size/行数）> minifiedAvgLineBytes。采样换行 ≥ 可疑区间上界
// 直接非 minified（正常文件，不做全文件统计）。
// 读失败/空文件/小文件按非 minified 处理（后续扫描分支会报真正的错误）。
func isMinified(env *Env, p string) bool {
	if isMinifiedName(p) {
		return true
	}
	info, err := env.VFS.Stat(p)
	if err != nil || info.Size() < minifiedMinBytes {
		return false
	}
	size := info.Size()
	n := size
	if n > minifiedHeadBytes {
		n = minifiedHeadBytes
	}
	f, err := env.VFS.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, n)
	if _, err := io.ReadFull(f, buf); err != nil && err != io.EOF {
		return false
	}
	sampleNL := bytes.Count(buf, []byte{'\n'})
	if sampleNL < minifiedMinLines {
		return true
	}
	if sampleNL >= minifiedSuspectLines {
		return false // 正常格式化文件，跳过全文件统计
	}
	// 可疑区间（头部含长 license 注释的压缩库等）：全文件平均行长兜底
	// （行数 = \n 计数，与 JS 端 indexOf 一致）
	var lines int64
	if size <= streamThreshold {
		data, err := env.VFS.ReadFile(p)
		if err != nil {
			return false
		}
		lines = int64(bytes.Count(data, []byte{'\n'}))
	} else {
		lines = int64(sampleNL)             // 采样段计数
		r := bufio.NewReaderSize(f, 64<<10) // 从采样偏移续扫
		for {
			line, err := r.ReadString('\n')
			if len(line) > 0 {
				lines++
			}
			if err != nil {
				break
			}
		}
	}
	return lines > 0 && size/lines > minifiedAvgLineBytes
}

// skipDirs 是 rg 递归不进入的目录（§5.4：node_modules/vendor 与常见编译产物/
// 缓存目录；与 ls skipDirs 同一集合）。
var skipDirs = map[string]bool{
	"node_modules": true, "vendor": true, "__pycache__": true,
	"bower_components": true, "dist": true, "build": true, "target": true,
	".next": true, ".nuxt": true, "coverage": true, ".turbo": true, ".output": true,
}

func fsRg(ctx context.Context, env *Env, p *fsParams) (*Result, error) {
	// glob：! 前缀 = 排除 glob（合法）；** 仍受限（跨目录语义不实现）
	for _, g := range p.Glob {
		if body := strings.TrimPrefix(g, "!"); strings.Contains(body, "**") {
			return nil, fsErr("rg", "glob %q is not supported on this environment (restricted: no '**')", g)
		}
	}
	// limit：全局输出行数上限（命中+上下文行同池；默认 50，上限 200）
	limit := rgDefaultLimit
	if p.Limit != nil {
		if *p.Limit < 1 || *p.Limit > rgMaxLimit {
			return nil, fsErr("rg", "limit must be between 1 and %d, got %d", rgMaxLimit, *p.Limit)
		}
		limit = *p.Limit
	}
	// context：命中行上下各 N 行（默认 0，上限 10）
	rgCtx := 0
	if p.Context != nil {
		if *p.Context < 0 || *p.Context > rgMaxContext {
			return nil, fsErr("rg", "context must be between 0 and %d, got %d", rgMaxContext, *p.Context)
		}
		rgCtx = *p.Context
	}

	target := env.Workdir
	if p.Path != "" {
		target = p.Path
	}

	// pattern 缺省 = 列举模式：纯文件列举（字节序，limit 上限）
	if p.Pattern == "" {
		if rgCtx > 0 {
			return nil, fsErr("rg", "context is only valid for content search (pattern is required)")
		}
		return rgFiles(ctx, env, target, p.Glob, p.All, limit, p.Depth)
	}

	for _, re := range rgUnsupportedPatterns {
		if re.MatchString(p.Pattern) {
			return nil, fsErr("rg", "%s", rgUnsupportedHint)
		}
	}
	// smart case：pattern 不含大写字母 → 大小写不敏感（ripgrep --smart-case 惯例）
	pattern := p.Pattern
	if !hasUpper(pattern) {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fsErr("rg", "invalid pattern: %s", err)
	}

	abs, err := env.Resolve(target)
	if err != nil {
		return nil, fsErr("rg", "%s", err)
	}
	if err := env.CheckPath("rg", abs); err != nil {
		return nil, err
	}
	if err := env.CheckPolicy("fs rg", abs, false); err != nil {
		return nil, err
	}
	info, err := env.VFS.Stat(abs)
	if err != nil {
		return nil, fsErr("rg", "%s", err)
	}
	// 候选文件集：单文件直搜（显式路径不受 glob 过滤，也不做 minified 跳过——
	// 用户显式指定文件即明确意图）；目录递归按字节序遍历（minified 默认跳过）。
	var candidates []string
	if !info.IsDir() {
		candidates = []string{abs}
		return rgSearch(env, abs, p.Pattern, candidates, re, limit, rgCtx, true)
	} else if err := rgWalk(ctx, env, abs, p.Glob, p.All, func(p string) { candidates = append(candidates, p) }); err != nil {
		return nil, fsErr("rg", "%s", err)
	}
	return rgSearch(env, abs, p.Pattern, candidates, re, limit, rgCtx, p.All)
}

// hasUpper 报告 pattern 是否含大写字母（smart case 判定）。
func hasUpper(s string) bool {
	for _, r := range s {
		if unicode.IsUpper(r) {
			return true
		}
	}
	return false
}

// rgWalk 递归收集目录下的文件（对齐真实 rg：默认跳过隐藏文件与隐藏目录，
// --hidden 收录；skipDirs 恒跳过；glob 按文件名过滤）。
func rgWalk(ctx context.Context, env *Env, dir string, globs []string, hidden bool, fn func(path string)) error {
	return rgWalkDepth(ctx, env, dir, globs, hidden, fn, 0)
}

func rgWalkDepth(ctx context.Context, env *Env, dir string, globs []string, hidden bool, fn func(path string), depth int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, err := env.VFS.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		hiddenName := strings.HasPrefix(name, ".")
		if e.IsDir() {
			if skipDirs[name] || (!hidden && hiddenName) {
				continue
			}
			if depth == 1 {
				continue
			}
			next := depth
			if next > 0 {
				next--
			}
			if err := rgWalkDepth(ctx, env, dir+"/"+name, globs, hidden, fn, next); err != nil {
				return err
			}
			continue
		}
		if hiddenName && !hidden {
			continue
		}
		if globOK(globs, name) {
			fn(dir + "/" + name)
		}
	}
	return nil
}

// globOK：include glob OR 任一命中即通过；! 前缀 = 排除 glob（命中任一
// 排除即不通过）；无 include = 全通过。** 在 fsRg 入口已拒绝。
func globOK(globs []string, name string) bool {
	var inc []string
	for _, g := range globs {
		if strings.HasPrefix(g, "!") {
			if globMatch(strings.TrimPrefix(g, "!"), name) {
				return false
			}
			continue
		}
		inc = append(inc, g)
	}
	if len(inc) == 0 {
		return true
	}
	for _, g := range inc {
		if globMatch(g, name) {
			return true
		}
	}
	return false
}

// rgFiles 实现 --files：递归列出文件（字节序，limit 上限）。
func rgFiles(ctx context.Context, env *Env, target string, globs []string, hidden bool, limit int, depths ...*int) (*Result, error) {
	depth := 0
	if len(depths) > 0 && depths[0] != nil {
		depth = *depths[0]
		if depth < 0 {
			return nil, fsErr("rg", "depth must be nonnegative")
		}
	}
	abs, err := env.Resolve(target)
	if err != nil {
		return nil, fsErr("rg", "%s", err)
	}
	if err := env.CheckPath("rg", abs); err != nil {
		return nil, err
	}
	if err := env.CheckPolicy("fs rg", abs, false); err != nil {
		return nil, err
	}
	info, err := env.VFS.Stat(abs)
	if err != nil {
		return nil, fsErr("rg", "%s", err)
	}
	var files []string
	if !info.IsDir() {
		// 单文件显式路径：直出（不受隐藏/glob 过滤，对齐真实 rg 显式路径语义）
		files = []string{abs}
	} else if err := rgWalkDepth(ctx, env, abs, globs, hidden, func(p string) { files = append(files, p) }, depth); err != nil {
		return nil, fsErr("rg", "%s", err)
	}
	// UTF-8 字节序排序（禁止 locale 相关排序，§5.4）
	sort.Strings(files)
	truncated := len(files) > limit
	if truncated {
		files = files[:limit]
	}

	r := newResult("rg", abs)
	jb := newRGJSONBuf(`]}`) // 预留数组+顶层闭合（2 字节）
	jb.write(`{"files":[`)
	rows := 0
	for i, f := range files {
		fj, err := json.Marshal(f)
		if err != nil {
			continue
		}
		s := string(fj)
		if i > 0 {
			s = "," + s // 逗号与元素原子写入，防悬空逗号
		}
		if !jb.write(s) {
			truncated = true
			break
		}
		rows++
	}
	jb.close(`]}`)
	r.Content = jb.done()
	r.set("rows", rows)
	r.set("truncated", truncated)
	return r, nil
}

// rgNoteMax 是 note 尾部的最坏情况长度（skipped 为 int64 最大值），用于
// rgSearch 的 rgJSONBuf 预留收尾空间。
const rgNoteMax = `],"note":"9223372036854775807 minified files skipped, use all=true to include"}`

// rgNoteFmt 是实际 note 尾部模板（skipped>0 时替换 %d）。
const rgNoteFmt = `],"note":"%d minified files skipped, use all=true to include"}`

// rgJSONBuf 是 rg JSON 输出的预算感知增量构建器：构建时预留尾部闭合
// （含可选 note）空间，内容写满即截断（write 返回 false）但 close 恒成功——
// 输出恒为合法 JSON 且总字节 ≤ MaxContentBytes。
type rgJSONBuf struct {
	b         strings.Builder
	tail      string
	limit     int
	truncated bool
}

func newRGJSONBuf(tail string) *rgJSONBuf {
	return &rgJSONBuf{tail: tail, limit: MaxContentBytes - len(tail)}
}

// write 追加内容；超预算返回 false 并置 truncated（此后 write 全部拒绝）。
func (j *rgJSONBuf) write(s string) bool {
	if j.truncated || j.b.Len()+len(s) > j.limit {
		j.truncated = true
		return false
	}
	j.b.WriteString(s)
	return true
}

// close 强制写入收尾（调用方保证 end 长度 ≤ 构造时预留的 tail）。
func (j *rgJSONBuf) close(end string) {
	j.b.WriteString(end)
}

func (j *rgJSONBuf) done() string {
	return j.b.String()
}

// rgRow 是单文件扫描的一条输出行（sep = 组间 -- 分隔行）。
type rgRow struct {
	sep   bool
	line  int
	text  string
	match bool
}

// rgEmit 单遍扫描+上下文展开（GNU grep -C 语义）：
//   - 命中行前 rgCtx 行经 pending 缓冲补发、后 rgCtx 行由 after 计数直发；
//   - 上下文区相邻或重叠（两命中之间无未选行）合并为一个组，组间插入 --；
//   - 内容行数（不含 --）达到 maxRows 后进入探测模式：继续扫描但不收集，
//     后续仍有命中才标记 truncated（文件恰好耗尽则不误标）。
func rgEmit(next func() (int, string, bool), re *regexp.Regexp, rgCtx, maxRows int) (rows []rgRow, truncated bool) {
	var pending []rgRow // before-context 缓冲（至多 rgCtx 行）
	lastEmitted := 0    // 最近已输出内容行号（0 = 组未开）
	after := 0
	n := 0
	for {
		num, text, ok := next()
		if !ok {
			break
		}
		isMatch := re.MatchString(text)
		if n >= maxRows {
			// 预算已满：只探测剩余行是否还有命中（决定 truncated）
			if isMatch {
				truncated = true
				break
			}
			continue
		}
		if isMatch {
			// 组间断判定：新命中（或其 before 缓冲首行）与上一输出行不衔接 → --
			start := num
			if len(pending) > 0 {
				start = pending[0].line
			}
			if rgCtx > 0 && lastEmitted > 0 && start != lastEmitted+1 {
				rows = append(rows, rgRow{sep: true})
			}
			for _, pr := range pending {
				if n >= maxRows {
					truncated = true
					break
				}
				rows = append(rows, pr)
				n++
				lastEmitted = pr.line
			}
			pending = nil
			if n >= maxRows {
				// 命中行自身放不下：截断（存在被压制的命中）
				truncated = true
				break
			}
			rows = append(rows, rgRow{line: num, text: text, match: true})
			n++
			lastEmitted = num
			after = rgCtx
			continue
		}
		if after > 0 {
			rows = append(rows, rgRow{line: num, text: text})
			n++
			lastEmitted = num
			after--
		} else if rgCtx > 0 {
			pending = append(pending, rgRow{line: num, text: text})
			if len(pending) > rgCtx {
				pending = pending[1:]
			}
		}
	}
	return rows, truncated
}

// rgSearch 实现内容搜索：逐文件扫描+上下文展开，全局 limit 截断。
// includeMinified=false 时 minified 文件（isMinified）跳过并计入 attrs.skipped
// （all=true 收录；显式单文件路径恒 includeMinified=true）。
func rgSearch(env *Env, abs, pattern string, candidates []string, re *regexp.Regexp, limit, rgCtx int, includeMinified bool) (*Result, error) {
	r := newResult("rg", abs)
	// 预留最大 note 空间：内容写满即截断，收尾（数组闭合+可选 note）恒可写
	jb := newRGJSONBuf(rgNoteMax)
	jb.write(`{"files":[`)
	contentRows := 0
	truncated := false
	clipped := false
	skipped := 0
	firstFile := true
	for _, f := range candidates {
		if contentRows >= limit {
			truncated = true
			break
		}
		if !includeMinified && isMinified(env, f) {
			skipped++
			continue
		}
		frows, ftrunc, err := rgFileRows(env, f, re, rgCtx, limit-contentRows)
		if err != nil || len(frows) == 0 {
			continue // 读不了的文件/二进制文件跳过
		}
		if ftrunc {
			truncated = true
		}
		// 文件级原子 chunk：逗号+整文件整体写入预算检查，超限丢弃（JSON 恒闭合）
		var fb strings.Builder
		fb.WriteString(`{"path":`)
		pj, err := json.Marshal(f)
		if err != nil {
			continue
		}
		fb.Write(pj)
		fb.WriteString(`,"matches":[`)
		fRows := 0
		firstRow := true
		for _, r := range frows {
			if r.sep {
				continue // 结构化后组间分隔无意义（行序即上下文序）
			}
			text := r.text
			if len(text) > rgMaxLineBytes {
				text = clipRgText(text)
				clipped = true
			}
			tj, err := json.Marshal(text)
			if err != nil {
				continue
			}
			if !firstRow {
				fb.WriteString(",")
			}
			firstRow = false
			fb.WriteString(`{"line":`)
			fb.WriteString(strconv.Itoa(r.line))
			fb.WriteString(`,"text":`)
			fb.Write(tj)
			if !r.match {
				fb.WriteString(`,"ctx":true`)
			}
			fb.WriteString(`}`)
			fRows++
		}
		if fRows == 0 {
			continue
		}
		chunk := fb.String() + `]}`
		if !firstFile {
			chunk = "," + chunk
		}
		if !jb.write(chunk) {
			truncated = true
			break
		}
		firstFile = false
		contentRows += fRows
	}
	if skipped > 0 {
		jb.close(fmt.Sprintf(rgNoteFmt, skipped))
	} else {
		jb.close(`]}`)
	}
	if clipped {
		truncated = true
	}
	r.Content = jb.done()
	r.set("rows", contentRows)
	r.set("truncated", truncated)
	if skipped > 0 {
		r.set("skipped", skipped)
	}
	return r, nil
}

// rgFileRows 扫描单文件：小文件整读逐行，>8MB 候选走流式（同一遍算法）。
// 二进制文件返回空（跳过）。
func rgFileRows(env *Env, p string, re *regexp.Regexp, rgCtx, maxRows int) ([]rgRow, bool, error) {
	info, err := env.VFS.Stat(p)
	if err != nil {
		return nil, false, err
	}

	if info.Size() <= streamThreshold {
		data, err := env.VFS.ReadFile(p)
		if err != nil || !isTextContent(data) {
			return nil, false, err
		}
		// 剥除尾随换行产生的空元素，避免 ^$ 等空串模式幻影报出文件末尾一行
		lines := strings.Split(string(data), "\n")
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		idx := 0
		next := func() (int, string, bool) {
			if idx >= len(lines) {
				return 0, "", false
			}
			i := idx
			idx++
			return i + 1, strings.TrimSuffix(lines[i], "\r"), true
		}
		rows, truncated := rgEmit(next, re, rgCtx, maxRows)
		return rows, truncated, nil
	}

	f, err := env.VFS.Open(p)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64<<10)
	head, _ := r.Peek(512)
	for n := 0; n < 3 && len(head) > 0 && !utf8.Valid(head); n++ {
		head = head[:len(head)-1]
	}
	if !isTextContent(head) {
		return nil, false, nil
	}
	lineNum := 0
	next := func() (int, string, bool) {
		line, _ := r.ReadString('\n')
		if len(line) == 0 {
			return 0, "", false
		}
		lineNum++
		return lineNum, strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"), true
	}
	rows, truncated := rgEmit(next, re, rgCtx, maxRows)
	return rows, truncated, nil
}
