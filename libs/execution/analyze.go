package execution

import (
	"strings"

	"github.com/veypi/vsh/shell/syntax"
)

// maxAnalyzeScriptDepth 脚本递归分析限深（bash x.sh / source / ./x.sh 套娃防爆；
// todo 2.5.3：2-3 层。TOCTOU 记录在案：分析时的脚本内容与执行时可能不同——
// 本分析仅作预检报错材料，拦截由运行期 FS 适配器规则表门兜底）。
const maxAnalyzeScriptDepth = 3

// Analysis 是脚本语法与字面授权申请的静态分析结果，不作权限判定。
type Analysis struct {
	// HasGrant detects a grant that may modify authorization. Status/help are
	// read-only; a dynamic subcommand still requires preflight approval.
	HasGrant bool
	// SyntaxError 语法错误（非空时 exec 直接返回，不进引擎）。
	SyntaxError string
}

// Analyze 只识别语法错误与字面 grant 授权申请，不推测文件写入。readFile 用于脚本递归
// （bash x.sh / sh x.sh / source x.sh / ./x.sh 的脚本正文读入后递归分析）；
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
		call, ok := stmt.Cmd.(*syntax.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		name, ok := wordLiteral(call.Args[0])
		if !ok {
			return true // 动态命令名跳过
		}
		args := literalArgsEach(call.Args[1:])
		// Match cmdGrant's read-only branches. Continue walking even for help
		// and status: argument substitutions may contain a separate grant request.
		if name == "grant" && len(args) > 0 &&
			(!args[0].ok || (args[0].lit != "status" && !helpRequested([]string{args[0].lit}))) {
			a.HasGrant = true
		}
		// 脚本递归：bash x.sh / sh x.sh / source x.sh / . x.sh / ./x.sh。
		if depth < maxAnalyzeScriptDepth && readFile != nil {
			var scriptPath string
			switch name {
			case "bash", "sh", "source", ".":
				if len(args) > 0 && args[0].ok {
					scriptPath = args[0].lit
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

// arg 是一个命令参数的字面判定（逐词独立，2026-09-24 F3 修复：原
// literalArgs 遇首个动态词截断，导致 `ln -s <字面target> $L` 里真正的末位
// 写目标丢失、倒数第二个字面词被误报为写目标）。
type arg struct {
	lit string
	ok  bool // 纯字面量；false = 含变量/替换/glob，静态层不猜（运行期门兜底）
}

// literalArgsEach 逐词提取字面判定（不截断——动态词只影响自己）。
func literalArgsEach(words []*syntax.Word) []arg {
	out := make([]arg, 0, len(words))
	for _, w := range words {
		lit, ok := wordLiteral(w)
		out = append(out, arg{lit, ok})
	}
	return out
}
