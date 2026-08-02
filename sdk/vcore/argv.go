package vcore

import "strings"

// argvSpec 声明一个虚拟指令的最精简 flag 子集（§5.4：未列 flag 报错）。
type argvSpec struct {
	bools  map[string]bool // 无值 flag：-i -la --files
	values map[string]bool // 带值 flag：-m -o --max-size
	lists  map[string]bool // 带值可重复 flag：-g（按出现顺序累积）
	minPos int
	maxPos int
}

type parsedArgv struct {
	bools  map[string]bool
	values map[string]string
	lists  map[string][]string
	pos    []string
}

// parseArgv 解析虚拟指令 argv（§5.4）：
//   - 未列 flag → 受限反馈 `{cmd}: flag "{flag}" is not supported on this environment (restricted)`；
//   - 单横线组合 flag（如 `-la`）：全部为已知无值 flag 时展开等价；
//   - `--flag=value` 形态不解析，按受限反馈引导两元素形式；
//   - 位置参数个数严格校验（多余报错，不得静默忽略）。
func parseArgv(cmd string, spec argvSpec, argv []string) (*parsedArgv, error) {
	out := &parsedArgv{bools: map[string]bool{}, values: map[string]string{}, lists: map[string][]string{}}
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if strings.HasPrefix(a, "-") && a != "-" {
			if strings.Contains(a, "=") {
				return nil, execErr(cmd, "flag %q is not supported on this environment (restricted; use \"--flag value\" two-element form)", a)
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
			if spec.lists[a] {
				if i+1 >= len(argv) {
					return nil, execErr(cmd, "flag %q requires a value", a)
				}
				i++
				out.lists[a] = append(out.lists[a], argv[i])
				continue
			}
			// 单横线组合（-la）：全部为已知无值 flag 时展开（对齐真实命令）
			if expanded, ok := expandBoolCombo(spec, a); ok {
				for _, f := range expanded {
					out.bools[f] = true
				}
				continue
			}
			return nil, execErr(cmd, "flag %q is not supported on this environment (restricted)", a)
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

// expandBoolCombo 展开单横线组合 flag（如 "-la" → "-l","-a"）：
// 每个字符都必须是已声明的无值 flag，否则 ok=false（按未知 flag 走受限反馈）。
// 双横线长 flag（--files）与单字符 flag 不展开。
func expandBoolCombo(spec argvSpec, a string) ([]string, bool) {
	if len(a) <= 2 || strings.HasPrefix(a, "--") {
		return nil, false
	}
	flags := make([]string, 0, len(a)-1)
	for _, c := range a[1:] {
		f := "-" + string(c)
		if !spec.bools[f] {
			return nil, false
		}
		flags = append(flags, f)
	}
	return flags, true
}

// globMatch 是 rg -g 的文件名 glob（§5.4：支持 * 任意序列与 ? 单字符，完整匹配）。
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
