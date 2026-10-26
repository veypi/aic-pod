// exec 域命令规则：解析与校验归 cfg（cfg 自己就要校验这些配置；
// 原 libs/policy 的命令规则部分，2026-10-06 批次 6 §4 合入）。
// FS/net 规则解析直接用 vbox。
package cfg

import (
	"fmt"
	"path"
	"strings"
)

// ParseExecRule accepts one exact command identity: a bare name or canonical absolute path.
func ParseExecRule(raw string) (allow bool, name string, err error) {
	effect, name, ok := strings.Cut(raw, ":")
	if !ok || (effect != "allow" && effect != "deny") || ValidateCommandName(name) != nil {
		return false, "", fmt.Errorf("invalid exec rule %q (want allow:name or deny:name)", raw)
	}
	return effect == "allow", name, nil
}

// ValidateCommandName 校验单个命令身份：裸名不含路径；绝对路径必须是
// 规范 POSIX 形（Windows 在引擎入口已转为 /c/...），不含空白、反斜杠或通配。
func ValidateCommandName(name string) error {
	if name == "" || strings.TrimSpace(name) != name || strings.ContainsAny(name, " \t\r\n\x00*?") {
		return fmt.Errorf("invalid exec command %q", name)
	}
	if strings.Contains(name, "\\") {
		return fmt.Errorf("invalid exec command %q", name)
	}
	if strings.Contains(name, "/") && (!path.IsAbs(name) || path.Clean(name) != name || name == "/") {
		return fmt.Errorf("invalid exec command path %q", name)
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

// CommandAllowed 按首命中判定命令准入。调用方可同时提供解析名与解析路径；
// 任一身份命中当前行即由该行决定，后续行不再读取。
func CommandAllowed(mode string, rows []string, names ...string) bool {
	if mode != "open" && mode != "deny" {
		return false
	}
	valid := false
	for _, name := range names {
		if ValidateCommandName(name) == nil {
			valid = true
			break
		}
	}
	if !valid {
		return false
	}
	for _, raw := range rows {
		allow, target, err := ParseExecRule(raw)
		if err != nil {
			return false
		}
		for _, name := range names {
			if ValidateCommandName(name) == nil && target == name {
				return allow
			}
		}
	}
	return mode == "open"
}
