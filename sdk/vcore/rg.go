package vcore

import (
	"bufio"
	"context"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ---- rg（§5.4）----
//
// rg 是 ripgrep CLI 子集（正则语义与 Rust regex 同族：线性时间、无回溯、
// 无 lookaround/backreference——命中拒绝清单时引导 bash -c "grep -P ..." 逃生舱）。
//
// 两种模式：
//
//	rg --files [-g GLOB]... [path]                 递归列出文件（字节序）
//	rg [-i] [-l] [-m N] [-g GLOB]... <pattern> <path>   内容搜索
//
// 输出为 rg 管道格式：内容搜索 {path}:{line}:{content}（行尾 \r 剥除）；
// -l 每命中文件输出路径一行；--files 每行一个文件路径。
//
// 平台行为（非 flag，真 rg 无对应项，文档明示）：全局 100 行上限 + truncated 标记、
// 512KB 输出预算、skipDirs 与点目录跳过、二进制文件跳过。
// -g glob 按文件名匹配（basename，* 任意序列 / ? 单字符，完整匹配），
// 多 -g 为 OR；不支持 ! 否定前缀与 ** 跨目录（真 rg 子集外的能力不引入）。

// rgUnsupportedPatterns 是 Rust regex 同族不支持的特性清单（lookaround/backreference），
// 与 JS 端校验清单一致：命中即显式报错并引导 shell 逃生舱。
var rgUnsupportedPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\(\?<?[=!]`), // (?= (?! (?<= (?<!
	regexp.MustCompile(`\\[1-9]`),    // \1..\9 backreference
}

const rgUnsupportedHint = `pattern uses unsupported feature (lookaround/backreference), use bash -c "grep -P ..." on a physical host`

// rgDefaultLimit 是全局输出行数平台上限（查越限标记 truncated）。
const rgDefaultLimit = 100

// skipDirs 是 rg 递归不进入的目录（§5.4：node_modules/vendor 与 . 开头目录）。
var skipDirs = map[string]bool{"node_modules": true, "vendor": true}

func cmdRg(ctx context.Context, env *Env, argv []string) (*Result, error) {
	pa, err := parseArgv("rg", argvSpec{
		bools:  map[string]bool{"--files": true, "-i": true, "-l": true},
		values: map[string]bool{"-m": true},
		lists:  map[string]bool{"-g": true},
		minPos: 0, maxPos: 2,
	}, argv)
	if err != nil {
		return nil, err
	}
	globs := pa.lists["-g"]

	// --files 模式：纯文件列举，不接受搜索 flag
	if pa.bools["--files"] {
		if pa.bools["-i"] || pa.bools["-l"] || pa.values["-m"] != "" {
			return nil, execErr("rg", "--files cannot be used with search flags (-i, -l, -m)")
		}
		if len(pa.pos) > 1 {
			return nil, execErr("rg", "unexpected argument %q", pa.pos[1])
		}
		target := env.Workdir
		if len(pa.pos) == 1 {
			target = pa.pos[0]
		}
		return rgFiles(ctx, env, target, globs)
	}

	// 搜索模式：pattern + path 两个位置参数
	if len(pa.pos) < 2 {
		return nil, execErr("rg", "missing argument: rg [OPTIONS] <pattern> <path>")
	}
	pattern := pa.pos[0]
	for _, re := range rgUnsupportedPatterns {
		if re.MatchString(pattern) {
			return nil, execErr("rg", "%s", rgUnsupportedHint)
		}
	}
	if pa.bools["-i"] {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, execErr("rg", "invalid pattern: %s", err)
	}
	maxPerFile := 0
	if v := pa.values["-m"]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, execErr("rg", "-m must be >= 1, got %s", v)
		}
		maxPerFile = n
	}

	abs, err := env.Resolve(pa.pos[1])
	if err != nil {
		return nil, execErr("rg", "%s", err)
	}
	info, err := env.VFS.Stat(abs)
	if err != nil {
		return nil, execErr("rg", "%s", err)
	}
	// 候选文件集：单文件直搜（显式路径不受 -g 过滤）；目录递归按字节序遍历。
	var candidates []string
	if !info.IsDir() {
		candidates = []string{abs}
	} else if err := rgWalk(ctx, env, abs, globs, func(p string) { candidates = append(candidates, p) }); err != nil {
		return nil, execErr("rg", "%s", err)
	}
	return rgSearch(env, abs, pa.pos[0], candidates, re, maxPerFile, pa.bools["-l"])
}

// rgWalk 递归收集目录下的文件（跳过 skipDirs 与点目录；glob 按文件名 OR 过滤）。
func rgWalk(ctx context.Context, env *Env, dir string, globs []string, fn func(path string)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, err := env.VFS.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			if skipDirs[name] || strings.HasPrefix(name, ".") {
				continue
			}
			if err := rgWalk(ctx, env, dir+"/"+name, globs, fn); err != nil {
				return err
			}
			continue
		}
		if globOK(globs, name) {
			fn(dir + "/" + name)
		}
	}
	return nil
}

// globOK：无 glob 全通过；多 glob 任一命中即通过（OR 语义）。
func globOK(globs []string, name string) bool {
	if len(globs) == 0 {
		return true
	}
	for _, g := range globs {
		if globMatch(g, name) {
			return true
		}
	}
	return false
}

// rgFiles 实现 --files：递归列出文件（字节序，平台上限 rgDefaultLimit）。
func rgFiles(ctx context.Context, env *Env, target string, globs []string) (*Result, error) {
	abs, err := env.Resolve(target)
	if err != nil {
		return nil, execErr("rg", "%s", err)
	}
	info, err := env.VFS.Stat(abs)
	if err != nil {
		return nil, execErr("rg", "%s", err)
	}
	var files []string
	if !info.IsDir() {
		if globOK(globs, path.Base(abs)) {
			files = []string{abs}
		}
	} else if err := rgWalk(ctx, env, abs, globs, func(p string) { files = append(files, p) }); err != nil {
		return nil, execErr("rg", "%s", err)
	}
	// UTF-8 字节序排序（禁止 locale 相关排序，§5.4）
	sort.Strings(files)
	truncated := len(files) > rgDefaultLimit
	if truncated {
		files = files[:rgDefaultLimit]
	}

	r := newResult("rg", abs)
	if len(files) == 0 {
		if len(globs) > 0 {
			r.Content = fmt.Sprintf("no files matched globs %s in %s", strings.Join(globs, ", "), abs)
		} else {
			r.Content = fmt.Sprintf("no files found in %s", abs)
		}
		r.set("rows", 0)
		r.set("truncated", false)
		return r, nil
	}
	var b strings.Builder
	rows := 0
	for _, f := range files {
		line := f + "\n"
		// 512KB 字节预算只留完整行（§2.5）
		if rows > 0 && b.Len()+len(line) > MaxContentBytes {
			truncated = true
			break
		}
		b.WriteString(line)
		rows++
	}
	r.Content = strings.TrimRight(b.String(), "\n")
	r.set("rows", rows)
	r.set("truncated", truncated)
	return r, nil
}

type rgMatch struct {
	path string
	line int
	text string
}

// rgSearch 实现内容搜索：逐文件匹配（-m 为每文件上限），全局 rgDefaultLimit 截断。
// filesOnly（-l）：每命中文件仅输出路径一行。
func rgSearch(env *Env, abs, pattern string, candidates []string, re *regexp.Regexp, maxPerFile int, filesOnly bool) (*Result, error) {
	var rows []string
	truncated := false
	for _, f := range candidates {
		if len(rows) >= rgDefaultLimit {
			truncated = true
			break
		}
		quota := rgDefaultLimit - len(rows)
		perFile := maxPerFile
		if filesOnly {
			perFile = 1
		}
		if perFile <= 0 || perFile > quota {
			perFile = quota
		}
		ms, err := rgFile(env, f, re, perFile)
		if err != nil || len(ms) == 0 {
			continue // 读不了的文件/二进制文件跳过
		}
		if filesOnly {
			rows = append(rows, f)
			continue
		}
		for _, m := range ms {
			rows = append(rows, fmt.Sprintf("%s:%d:%s", m.path, m.line, m.text))
		}
	}

	r := newResult("rg", abs)
	if len(rows) == 0 {
		r.Content = fmt.Sprintf("no matches for pattern %q in %s", pattern, abs)
		r.set("rows", 0)
		r.set("truncated", false)
		return r, nil
	}
	var b strings.Builder
	out := 0
	for _, row := range rows {
		line := row + "\n"
		if out > 0 && b.Len()+len(line) > MaxContentBytes {
			truncated = true
			break
		}
		b.WriteString(line)
		out++
	}
	r.Content = strings.TrimRight(b.String(), "\n")
	r.set("rows", out)
	r.set("truncated", truncated)
	return r, nil
}

// rgFile 匹配单文件（至多 max 条）；二进制文件返回空（跳过）；
// 候选 >8MB 走嗅探流式匹配。
func rgFile(env *Env, p string, re *regexp.Regexp, max int) ([]rgMatch, error) {
	info, err := env.VFS.Stat(p)
	if err != nil {
		return nil, err
	}
	matchLine := func(line string) (string, bool) {
		line = strings.TrimSuffix(line, "\r") // CRLF 统一剥除（§5.4）
		return line, re.MatchString(line)
	}

	if info.Size() <= streamThreshold {
		data, err := env.VFS.ReadFile(p)
		if err != nil || !isTextContent(data) {
			return nil, err
		}
		// 剥除尾随换行产生的空元素，避免 ^$ 等空串模式幻影报出文件末尾一行
		lines := strings.Split(string(data), "\n")
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		var out []rgMatch
		for i, line := range lines {
			if text, ok := matchLine(line); ok {
				out = append(out, rgMatch{p, i + 1, text})
				if len(out) >= max {
					break
				}
			}
		}
		return out, nil
	}

	f, err := env.VFS.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64<<10)
	head, _ := r.Peek(512)
	for n := 0; n < 3 && len(head) > 0 && !utf8.Valid(head); n++ {
		head = head[:len(head)-1]
	}
	if !isTextContent(head) {
		return nil, nil
	}
	var out []rgMatch
	lineNum := 0
	for {
		line, err := r.ReadString('\n')
		if len(line) > 0 {
			lineNum++
			line = strings.TrimSuffix(line, "\n")
			if text, ok := matchLine(line); ok {
				out = append(out, rgMatch{p, lineNum, text})
				if len(out) >= max {
					return out, nil
				}
			}
		}
		if err != nil {
			return out, nil
		}
	}
}
