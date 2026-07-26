package aichost

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// parsedArgv 是 Unix 风格 argv 的解析结果：位置参数、--flag value、--bool-flag。
type parsedArgv struct {
	positional []string
	flags      map[string]string
	bools      map[string]bool
}

func (pa *parsedArgv) getInt(name string) int {
	v, _ := strconv.Atoi(pa.flags[name])
	return v
}

// flagKind 声明 action 级 flag 的类型（§2.1 第二层）。
type flagKind int

const (
	flagBool  flagKind = iota // 不消费值
	flagValue                 // 消费下一元素为值
)

// flagSet 是 action 级已知 flag 表：flag 名（不含 "--" 前缀）→ 类型。
type flagSet map[string]flagKind

// parseActionArgv 按 §2.1 双层规范严格解析 argv，与 aic libs/tools.ParseActionArgv 逐行为一致：
//   - "--" 终止符：其后所有元素一律进入 positional
//   - 以 "--" 开头且去前缀后匹配 ^[a-zA-Z][a-zA-Z0-9_-]*$ 的元素为候选 flag
//   - "--name=value" 且 name 部分为合法 flag 名 → 统一报 invalid flag 错误
//   - 未知 flag → unknown flag 错误（附 supported 列表）
//   - value flag 消费下一非候选 flag、非 "--" 元素为值，否则报 requires a value
//   - bool flag 不消费下一元素
//
// 错误消息统一 "{tool} {action}: {原因}" 格式。
func parseActionArgv(tool, action string, argv []string, flags flagSet) (*parsedArgv, error) {
	pa := &parsedArgv{
		flags: make(map[string]string),
		bools: make(map[string]bool),
	}
	terminated := false
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if terminated {
			pa.positional = append(pa.positional, arg)
			continue
		}
		if arg == "--" {
			terminated = true
			continue
		}
		key, ok := strings.CutPrefix(arg, "--")
		if !ok {
			pa.positional = append(pa.positional, arg)
			continue
		}
		// "--name=value" 形态：= 前部分为合法 flag 名时统一报错
		if name, _, hasEq := strings.Cut(key, "="); hasEq && looksLikeFlagName(name) {
			return nil, fmt.Errorf("%s %s: invalid flag %q; use \"--%s <value>\" (two separate argv elements)", tool, action, arg, name)
		}
		if !looksLikeFlagName(key) {
			pa.positional = append(pa.positional, arg)
			continue
		}
		kind, known := flags[key]
		if !known {
			return nil, fmt.Errorf("%s %s: unknown flag \"--%s\"%s", tool, action, key, supportedFlagsSuffix(flags))
		}
		if kind == flagBool {
			pa.bools[key] = true
			continue
		}
		if i+1 < len(argv) && argv[i+1] != "--" && !looksLikeCandidateFlag(argv[i+1]) {
			pa.flags[key] = argv[i+1]
			i++
			continue
		}
		return nil, fmt.Errorf("%s %s: flag \"--%s\" requires a value", tool, action, key)
	}
	return pa, nil
}

func supportedFlagsSuffix(flags flagSet) string {
	if len(flags) == 0 {
		return ""
	}
	names := make([]string, 0, len(flags))
	for name := range flags {
		names = append(names, "--"+name)
	}
	sort.Strings(names)
	return " (supported: " + strings.Join(names, ", ") + ")"
}

// looksLikeCandidateFlag 判定元素是否为候选 flag（第一层规则）。
func looksLikeCandidateFlag(arg string) bool {
	key, ok := strings.CutPrefix(arg, "--")
	if !ok {
		return false
	}
	if name, _, hasEq := strings.Cut(key, "="); hasEq {
		return looksLikeFlagName(name)
	}
	return looksLikeFlagName(key)
}

// looksLikeFlagName 判断 key（去掉 "--" 前缀后的部分）是否是合法标志名：
// 须以字母开头，后接字母、数字、'-'、'_'（^[a-zA-Z][a-zA-Z0-9_-]*$）。
func looksLikeFlagName(key string) bool {
	if key == "" {
		return false
	}
	for i, r := range key {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9' && i > 0:
		case (r == '-' || r == '_') && i > 0:
		default:
			return false
		}
	}
	return true
}
