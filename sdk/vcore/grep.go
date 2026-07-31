package vcore

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ---- grep（§5.4）----

// grepUnsupportedPatterns 是 RE2 公共子集之外的特性清单（lookaround/backreference），
// 与 JS 端校验清单一致：命中即显式报错并引导 shell 逃生舱。
var grepUnsupportedPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\(\?<?[=!]`), // (?= (?! (?<= (?<!
	regexp.MustCompile(`\\[1-9]`),    // \1..\9 backreference
}

const grepUnsupportedHint = `pattern uses unsupported feature, use bash -c "grep -P ..." on a physical host`

// grep [-r] [-i] [-m N] <pattern> <path>：RE2 公共子集正则；
// 输出 {path}:{行号}\t{行内容}（行号 1 基）；-m 默认 100（查 m+1 判定截断）；
// 行尾 \r 统一剥除；候选文件 >8MB 嗅探流式匹配。
func cmdGrep(ctx context.Context, env *Env, argv []string) (*Result, error) {
	pa, err := parseArgv("grep", argvSpec{
		bools:  map[string]bool{"-r": true, "-i": true},
		values: map[string]bool{"-m": true},
		minPos: 2, maxPos: 2,
	}, argv)
	if err != nil {
		return nil, err
	}
	max := 100
	if v := pa.values["-m"]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, execErr("grep", "-m must be >= 1, got %s", v)
		}
		max = n
	}
	pattern := pa.pos[0]
	for _, re := range grepUnsupportedPatterns {
		if re.MatchString(pattern) {
			return nil, execErr("grep", "%s", grepUnsupportedHint)
		}
	}
	if pa.bools["-i"] {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, execErr("grep", "invalid pattern: %s", err)
	}

	abs, err := env.Resolve(pa.pos[1])
	if err != nil {
		return nil, execErr("grep", "%s", err)
	}
	info, err := env.VFS.Stat(abs)
	if err != nil {
		return nil, execErr("grep", "%s", err)
	}

	// 候选文件集：单文件（无需 -r）；目录需 -r，walk 按路径字节序（向量锁定）
	var candidates []string
	if !info.IsDir() {
		candidates = []string{abs}
	} else {
		if !pa.bools["-r"] {
			return nil, execErr("grep", "%s is a directory (use -r)", abs)
		}
		var walk func(dir string) error
		walk = func(dir string) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			entries, err := env.VFS.ReadDir(dir)
			if err != nil {
				return err
			}
			for _, e := range entries {
				name := e.Name()
				if e.IsDir() && (skipDirs[name] || strings.HasPrefix(name, ".")) {
					continue
				}
				full := dir + "/" + name
				if e.IsDir() {
					if err := walk(full); err != nil {
						return err
					}
				} else {
					candidates = append(candidates, full)
				}
			}
			return nil
		}
		if err := walk(abs); err != nil {
			return nil, execErr("grep", "%s", err)
		}
	}

	// 逐文件逐行匹配，查 m+1 判定截断
	var matches []grepMatch
	for _, f := range candidates {
		if len(matches) > max {
			break
		}
		ms, err := grepFile(env, f, re, max+1-len(matches))
		if err != nil {
			continue // 读不了的文件跳过
		}
		matches = append(matches, ms...)
	}
	truncated := len(matches) > max
	if truncated {
		matches = matches[:max]
	}

	r := newResult("grep", abs)
	if len(matches) == 0 {
		r.Content = fmt.Sprintf("no lines matched pattern %q in %s", pa.pos[0], abs)
		r.set("rows", 0)
		r.set("truncated", false)
		return r, nil
	}
	var b strings.Builder
	rows := 0
	for _, m := range matches {
		row := fmt.Sprintf("%s:%d\t%s\n", m.path, m.line, m.text)
		if rows > 0 && b.Len()+len(row) > MaxContentBytes {
			truncated = true
			break
		}
		b.WriteString(row)
		rows++
	}
	r.Content = strings.TrimRight(b.String(), "\n")
	r.set("rows", rows)
	r.set("truncated", truncated)
	return r, nil
}

type grepMatch struct {
	path string
	line int
	text string
}

func grepFile(env *Env, p string, re *regexp.Regexp, max int) ([]grepMatch, error) {
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
		var out []grepMatch
		for i, line := range strings.Split(string(data), "\n") {
			if text, ok := matchLine(line); ok {
				out = append(out, grepMatch{p, i + 1, text})
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
	var out []grepMatch
	lineNum := 0
	for {
		line, err := r.ReadString('\n')
		if len(line) > 0 {
			lineNum++
			line = strings.TrimSuffix(line, "\n")
			if text, ok := matchLine(line); ok {
				out = append(out, grepMatch{p, lineNum, text})
				if len(out) >= max {
					return out, nil
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				return out, nil
			}
			return out, nil
		}
	}
}
