package vsh

import (
	"strings"

	"github.com/veypi/vsh/shell/syntax"
)

// maxAnalyzeScriptDepth 脚本递归分析限深（bash x.sh / source / ./x.sh 套娃防爆；
// todo 2.5.3：2-3 层。TOCTOU 记录在案：分析时的脚本内容与执行时可能不同——
// 本分析仅作预检报错材料，拦截由运行期 FS 适配器规则表门兜底）。
const maxAnalyzeScriptDepth = 3

// Analysis 是脚本静态分析结果（design §4 analyze.go；两端共用）。
// 只收集字面量目标——含变量/glob 的动态目标跳过（运行期由 FS 适配器
// 规则表门强制，验收 4 的 rm $X 拒绝在运行期发生）。
type Analysis struct {
	// WriteTargets 字面写目标（重定向 + 写参表命中的命令参数）。
	WriteTargets []string
	// UsesNetwork 是否使用网络命令（curl/wget——审计提示用，不升档不审批）。
	UsesNetwork bool
	// SyntaxError 语法错误（非空时 exec 直接返回，不进引擎）。
	SyntaxError string
}

// Analyze 解析脚本并收集字面写目标与网络使用。readFile 用于脚本递归
//（bash x.sh / sh x.sh / source x.sh / ./x.sh 的脚本正文读入后递归分析）；
// nil = 不递归（仅分析本脚本）。
func Analyze(script string, readFile func(path string) ([]byte, error)) Analysis {
	var a Analysis
	parser := syntax.NewParser()
	f, err := parser.Parse(strings.NewReader(script), "exec")
	if err != nil {
		a.SyntaxError = err.Error()
		return a
	}
	seen := map[string]bool{}
	a.walk(f, readFile, seen, 0)
	return a
}

func (a *Analysis) walk(f *syntax.File, readFile func(string) ([]byte, error), seen map[string]bool, depth int) {
	syntax.Walk(f, func(node syntax.Node) bool {
		stmt, ok := node.(*syntax.Stmt)
		if !ok {
			return true
		}
		// 重定向写目标（> >> &> &>>；< << 是读/输入不算写）。
		for _, redir := range stmt.Redirs {
			switch redir.Op {
			case syntax.RdrOut, syntax.AppOut, syntax.RdrAll, syntax.AppAll:
				if lit, ok := wordLiteral(redir.Word); ok {
					a.addWrite(lit)
				}
			}
		}
		call, ok := stmt.Cmd.(*syntax.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		name, ok := wordLiteral(call.Args[0])
		if !ok {
			return true // 动态命令名跳过
		}
		args := literalArgs(call.Args[1:])
		// 网络使用（cloud 仅 curl 注册；wget/host 端 native 同名单收录）。
		switch name {
		case "curl", "wget":
			a.UsesNetwork = true
		}
		// 写参表。
		if fn, ok := writeArgTable[name]; ok {
			for _, p := range fn(args) {
				a.addWrite(p)
			}
		}
		// 脚本递归：bash x.sh / sh x.sh / source x.sh / . x.sh / ./x.sh。
		if depth < maxAnalyzeScriptDepth && readFile != nil {
			var scriptPath string
			switch name {
			case "bash", "sh", "source", ".":
				if len(args) > 0 {
					scriptPath = args[0]
				}
			default:
				if strings.HasPrefix(name, "./") || (strings.Contains(name, "/") && strings.HasSuffix(name, ".sh")) {
					scriptPath = name
				}
			}
			if scriptPath != "" && !seen[scriptPath] {
				seen[scriptPath] = true
				if data, err := readFile(scriptPath); err == nil {
					if sub, perr := syntax.NewParser().Parse(strings.NewReader(string(data)), scriptPath); perr == nil {
						a.walk(sub, readFile, seen, depth+1)
					}
				}
			}
		}
		return true
	})
}

func (a *Analysis) addWrite(p string) {
	if p == "" || p == "-" {
		return
	}
	for _, existing := range a.WriteTargets {
		if existing == p {
			return
		}
	}
	a.WriteTargets = append(a.WriteTargets, p)
}

// wordLiteral 提取纯字面 Word（Lit/SglQuoted/全 Lit 的 DblQuoted 拼接）；
// 含变量/命令替换/glob 等动态部分 → ok=false（运行期强制，不在静态层猜）。
func wordLiteral(w *syntax.Word) (string, bool) {
	if w == nil {
		return "", false
	}
	var b strings.Builder
	for _, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			b.WriteString(p.Value)
		case *syntax.SglQuoted:
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, inner := range p.Parts {
				lit, ok := inner.(*syntax.Lit)
				if !ok {
					return "", false
				}
				b.WriteString(lit.Value)
			}
		default:
			return "", false
		}
	}
	return b.String(), true
}

// literalArgs 提取字面参数序列（遇非字面参数停止——其后参数归属已不可静态判定）。
func literalArgs(words []*syntax.Word) []string {
	out := make([]string, 0, len(words))
	for _, w := range words {
		lit, ok := wordLiteral(w)
		if !ok {
			break
		}
		out = append(out, lit)
	}
	return out
}

// --- 写参表（design M2 估足工作量）：命令 → 从字面参数中提取写目标 ---
// 只处理字面参数；flag 值跳过（-o/-f/of= 等按各自语义取）。
var writeArgTable = map[string]func(args []string) []string{
	// cp/mv/ln/install/rsync：末位 positional 是写目标。
	"cp":      lastPositional,
	"mv":      lastPositional,
	"ln":      lastPositional,
	"install": lastPositional,
	"rsync":   lastPositional,
	// tee：全部 positional 都是写目标。
	"tee": allPositionals,
	// 创建/删除类：全部 positional。
	"mkdir":    allPositionals,
	"touch":    allPositionals,
	"rm":       allPositionals,
	"rmdir":    allPositionals,
	"truncate": allPositionals,
	// 元数据写：首参是 mode/owner，其余 positional 是目标。
	"chmod": skipFirstPositional,
	"chown": skipFirstPositional,
	"chgrp": skipFirstPositional,
	// tar：-f/--file 的值（打包写归档、解包写目录——保守都收）。
	"tar": flagValueTargets("-f", "--file"),
	// sed -i：positional 是写目标；无 -i 不写文件。
	"sed": sedTargets,
	// curl/wget：-o/-O/--output 的值是写目标。
	"curl": flagValueTargets("-o", "--output", "-O", "--remote-name-all"),
	"wget": flagValueTargets("-O", "--output-document"),
	// dd：of= 值。
	"dd": ddTargets,
}

// positional 拆分：flag（-/-- 开头）与值分开；-- 后全为 positional。
// 注意：表内命令的带值 flag 须在 valueFlags 登记，否则其值会被误当 positional。
var valueFlags = map[string]bool{
	"-f": true, "--file": true, "-o": true, "-O": true, "--output": true,
	"--output-document": true, "-t": true, "--target-directory": true,
	"-C": true, "--directory": true, "-m": true, "--mode": true,
}

func splitPositionals(args []string) (positionals []string) {
	noMoreFlags := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if noMoreFlags {
			positionals = append(positionals, a)
			continue
		}
		if a == "--" {
			noMoreFlags = true
			continue
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			if valueFlags[a] {
				i++ // 跳过 flag 值
			}
			continue
		}
		positionals = append(positionals, a)
	}
	return positionals
}

func lastPositional(args []string) []string {
	pos := splitPositionals(args)
	if len(pos) == 0 {
		return nil
	}
	return []string{pos[len(pos)-1]}
}

func allPositionals(args []string) []string { return splitPositionals(args) }

func skipFirstPositional(args []string) []string {
	pos := splitPositionals(args)
	if len(pos) <= 1 {
		return nil
	}
	return pos[1:]
}

func flagValueTargets(flags ...string) func(args []string) []string {
	set := map[string]bool{}
	for _, f := range flags {
		set[f] = true
	}
	return func(args []string) []string {
		var out []string
		for i := 0; i < len(args); i++ {
			if set[args[i]] && i+1 < len(args) {
				out = append(out, args[i+1])
				i++
				continue
			}
			// --output=x 形态
			if strings.HasPrefix(args[i], "--") {
				if idx := strings.Index(args[i], "="); idx > 0 && set[args[i][:idx]] {
					out = append(out, args[i][idx+1:])
				}
			}
		}
		return out
	}
}

func sedTargets(args []string) []string {
	inPlace := false
	for _, a := range args {
		if a == "-i" || strings.HasPrefix(a, "-i") || a == "--in-place" {
			inPlace = true
			break
		}
	}
	if !inPlace {
		return nil
	}
	pos := splitPositionals(args)
	if len(pos) <= 1 {
		return nil
	}
	return pos[1:] // 首 positional 是脚本表达式
}

func ddTargets(args []string) []string {
	for _, a := range args {
		if strings.HasPrefix(a, "of=") {
			return []string{strings.TrimPrefix(a, "of=")}
		}
	}
	return nil
}
