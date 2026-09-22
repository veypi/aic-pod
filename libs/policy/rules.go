// Package policy defines the common execution-policy primitives. It has no
// session approval state and cannot elevate a call.
package policy

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ---- 有序规则表（docs/permission_rules.md §1）：行首效果前缀 + 全域模式禁写 ----
//
// 每域一张字符串表，一行一条规则，按书写顺序逐条匹配、最后命中者胜；
// 全表未命中走 *_policy 兜底姿态。fs 域效果：deny（读写双拒）/ ro（读开放写拒）/
// rw（读写）；net/ssh 域效果：allow / deny。

// fs 规则效果（行首白名单前缀）。
const (
	EffectDeny = "deny"
	EffectRO   = "ro"
	EffectRW   = "rw"
)

var fsEffectPrefixes = []struct{ prefix, effect string }{
	{"deny:", EffectDeny}, {"ro:", EffectRO}, {"rw:", EffectRW},
}

// ParseFSRule 解析一条 fs 规则行：白名单化剥取行首效果前缀（deny:/ro:/rw:），
// 其余一律按字面路径（保护 C:/ 盘符——"C:" 不在白名单，整行按缺前缀报错）。
// 全域模式禁写（§1）：放行类（ro/rw）禁字面全域、家目录根与整盘根；
// deny 禁字面全域——全域姿态只用 fs_policy 表达。
func ParseFSRule(raw string) (effect, pattern string, err error) {
	s := strings.TrimSpace(raw)
	for _, p := range fsEffectPrefixes {
		if strings.HasPrefix(s, p.prefix) {
			effect, pattern = p.effect, strings.TrimSpace(strings.TrimPrefix(s, p.prefix))
			break
		}
	}
	if effect == "" {
		return "", "", fmt.Errorf("invalid fs rule %q: missing effect prefix (deny: / ro: / rw:)", raw)
	}
	if pattern == "" || strings.ContainsRune(pattern, 0) {
		return "", "", fmt.Errorf("invalid fs rule %q: empty or invalid pattern", raw)
	}
	if err := checkGlobalFS(effect, pattern); err != nil {
		return "", "", fmt.Errorf("invalid fs rule %q: %v", raw, err)
	}
	return effect, pattern, nil
}

// checkGlobalFS 全域模式禁写（§1）：字面全域三形态全效果禁写（一条放行全域
// 拆光整张表含 builtin 凭证 deny；deny 全域与 fs_policy 同义，保持单一表达）；
// 放行类另禁家目录根与整盘根。报错信息指向 fs_policy 或更细条目。
func checkGlobalFS(effect, pattern string) error {
	p := strings.TrimSpace(pattern)
	if isLiteralGlobal(p) {
		return fmt.Errorf("global pattern is not expressible as a rule (use fs_policy for the whole-stance, or a narrower entry)")
	}
	if effect == EffectDeny {
		return nil
	}
	if p == "~" || p == "~/" || p == "~/**" {
		return fmt.Errorf("home directory root is not expressible as an allow rule (use fs_policy: open or narrower entries)")
	}
	if home, err := os.UserHomeDir(); err == nil {
		if h := filepath.ToSlash(home); p == h || p == h+"/" || p == h+"/**" {
			return fmt.Errorf("home directory root is not expressible as an allow rule (use fs_policy: open or narrower entries)")
		}
	}
	if driveRootRe.MatchString(strings.ReplaceAll(p, "\\", "/")) {
		return fmt.Errorf("drive root is not expressible as an allow rule (use narrower entries)")
	}
	return nil
}

func isLiteralGlobal(p string) bool {
	return p == "/" || p == "**" || p == "/**"
}

// driveRootRe 匹配整盘根三形态：C: / C:/ / C:/**（大小写不限；配置跨平台共享，统一禁写）。
var driveRootRe = regexp.MustCompile(`(?i)^[a-z]:(/\*\*)?/?$`)

// ValidateFSGrantTarget 校验 grant fs 目标（具体绝对路径，非模式）——
// temp/permanent grant 与规则行同护栏（§1）：全域/家根/盘根不可授。
func ValidateFSGrantTarget(abs string) error {
	p := filepath.ToSlash(strings.TrimSpace(abs))
	if p == "" || strings.ContainsRune(p, 0) {
		return fmt.Errorf("invalid grant target %q", abs)
	}
	if p == "/" {
		return fmt.Errorf("filesystem root cannot be granted (use fs_policy: open or narrower paths)")
	}
	if home, err := os.UserHomeDir(); err == nil {
		if p == filepath.ToSlash(home) {
			return fmt.Errorf("home directory root cannot be granted (use fs_policy: open or narrower paths)")
		}
	}
	if driveRootRe.MatchString(strings.ReplaceAll(p, "\\", "/")) {
		return fmt.Errorf("drive root cannot be granted (use narrower paths)")
	}
	return nil
}

// ValidateFSRules 批量校验 fs 规则行（加载/入表即报错；首个非法行即返回）。
func ValidateFSRules(list []string) error {
	for _, raw := range list {
		if _, _, err := ParseFSRule(raw); err != nil {
			return err
		}
	}
	return nil
}

// ParseTargetRule 解析 net/ssh 规则行：剥 allow:/deny: 前缀后按 Entry 归一。
// Entry 解析本身拒绝通配 host（含 "*"），allow:* / deny:* 等全域行在此自然报错（§1 指向 *_policy）。
func ParseTargetRule(raw string) (allow bool, e Entry, err error) {
	s := strings.TrimSpace(raw)
	switch {
	case strings.HasPrefix(s, "allow:"):
		allow, s = true, strings.TrimSpace(strings.TrimPrefix(s, "allow:"))
	case strings.HasPrefix(s, "deny:"):
		s = strings.TrimSpace(strings.TrimPrefix(s, "deny:"))
	default:
		return false, Entry{}, fmt.Errorf("invalid target rule %q: missing effect prefix (allow: / deny:)", raw)
	}
	e, err = ParseEntry(s)
	if err != nil {
		return false, Entry{}, err
	}
	return allow, e, nil
}

// ValidateTargetRules 批量校验 net/ssh 规则行（加载/入表即报错；首个非法行即返回）。
func ValidateTargetRules(list []string) error {
	for _, raw := range list {
		if _, _, err := ParseTargetRule(raw); err != nil {
			return err
		}
	}
	return nil
}

func ValidateExec(entries []string) error {
	for _, name := range entries {
		if name == "" || strings.TrimSpace(name) != name || strings.ContainsAny(name, "/\\ \t\r\n\x00") || (name != "*" && strings.ContainsAny(name, "*?")) {
			return fmt.Errorf("invalid exec command %q", name)
		}
	}
	return nil
}
func CommandAllowed(mode string, deny, allow []string, name string) bool {
	hit := func(list []string) bool {
		for _, p := range list {
			if p == "*" || p == name {
				return true
			}
		}
		return false
	}
	if mode != "open" && mode != "deny" {
		return false
	}
	return !hit(deny) && (hit(allow) || mode == "open")
}
