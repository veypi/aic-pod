package vcore

import "strings"

// argvSpec 声明一个虚拟指令的最精简 flag 子集（§5.4：未列 flag 报错）。
type argvSpec struct {
	bools  map[string]bool // 无值 flag：-r -i -la -p
	values map[string]bool // 带值 flag：-name -m -o --max-size
	minPos int
	maxPos int
}

type parsedArgv struct {
	bools  map[string]bool
	values map[string]string
	pos    []string
}

// parseArgv 解析虚拟指令 argv（§5.4）：
//   - 未列 flag → `{cmd}: unknown flag "{flag}"`；
//   - `--flag=value` 形态不解析，按非法 flag 报错引导两元素形式；
//   - 位置参数个数严格校验（多余报错，不得静默忽略）。
func parseArgv(cmd string, spec argvSpec, argv []string) (*parsedArgv, error) {
	out := &parsedArgv{bools: map[string]bool{}, values: map[string]string{}}
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if strings.HasPrefix(a, "-") && a != "-" {
			if strings.Contains(a, "=") {
				return nil, execErr(cmd, "unknown flag %q (use \"--flag value\" two-element form)", a)
			}
			if spec.bools[a] {
				out.bools[a] = true
				continue
			}
			if spec.values[a] {
				if i+1 >= len(argv) {
					return nil, execErr(cmd, "flag %q requires a value", a)
				}
				i++
				out.values[a] = argv[i]
				continue
			}
			return nil, execErr(cmd, "unknown flag %q", a)
		}
		out.pos = append(out.pos, a)
	}
	if len(out.pos) < spec.minPos {
		return nil, execErr(cmd, "missing argument (expected at least %d, got %d)", spec.minPos, len(out.pos))
	}
	if spec.maxPos >= 0 && len(out.pos) > spec.maxPos {
		return nil, execErr(cmd, "unexpected argument %q", out.pos[spec.maxPos])
	}
	return out, nil
}

// globMatch 是 find -name 的文件名 glob（§5.4：支持 * 任意序列与 ? 单字符，完整匹配）。
// 线性双指针：O(len(s)) 时间、O(1) 额外空间。
func globMatch(pattern, s string) bool {
	p, t := []rune(pattern), []rune(s)
	i, j := 0, 0
	star, mark := -1, 0
	for i < len(t) {
		switch {
		case j < len(p) && (p[j] == '?' || p[j] == t[i]):
			i++
			j++
		case j < len(p) && p[j] == '*':
			star, mark = j, i
			j++
		case star >= 0:
			mark++
			i = mark
			j = star + 1
		default:
			return false
		}
	}
	for j < len(p) && p[j] == '*' {
		j++
	}
	return j == len(p)
}
