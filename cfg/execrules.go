// exec 域命令规则：解析与校验归 cfg（cfg 自己就要校验这些配置；
// 原 libs/policy 的命令规则部分，2026-10-06 批次 6 §4 合入）。
// FS/net 规则解析直接用 vbox。
package cfg

import (
	"fmt"
	"strings"
)

// ParseExecRule accepts one exact name. Whole-domain behavior belongs to policy.
func ParseExecRule(raw string) (allow bool, name string, err error) {
	effect, name, ok := strings.Cut(raw, ":")
	if !ok || (effect != "allow" && effect != "deny") || ValidateCommandName(name) != nil {
		return false, "", fmt.Errorf("invalid exec rule %q (want allow:name or deny:name)", raw)
	}
	return effect == "allow", name, nil
}

// ValidateCommandName 校验单个命令名（无路径/空白/通配）。
func ValidateCommandName(name string) error {
	if name == "" || strings.TrimSpace(name) != name || strings.ContainsAny(name, "/\\ \t\r\n\x00*?") {
		return fmt.Errorf("invalid exec command %q", name)
	}
	return nil
}

// ValidateExecRules 校验 exec_rules 全表。
func ValidateExecRules(rows []string) error {
	for _, raw := range rows {
		if _, _, err := ParseExecRule(raw); err != nil {
			return err
		}
	}
	return nil
}

// CommandAllowed 按首命中判定命令准入（rows 有序，先判先赢）。
func CommandAllowed(mode string, rows []string, name string) bool {
	if (mode != "open" && mode != "deny") || ValidateCommandName(name) != nil {
		return false
	}
	for _, raw := range rows {
		allow, target, err := ParseExecRule(raw)
		if err != nil {
			return false
		}
		if target == name {
			return allow
		}
	}
	return mode == "open"
}
