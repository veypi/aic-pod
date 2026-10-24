package execution

// cloudpath.go 提供路径别名重定向的共享实现（ApplyAlias）：ufsAdapter 在
// resolve 期使用，cloud 调用路径策略（CloudPathPolicy）已移到 AIC。

import (
	"sort"
	"strings"
)

// ApplyAlias 把绝对路径按最长前缀命中重定向到别名目标。
// 例：{"/tmp": "/u/u1/.sessions/s1/tmp"} 时 /tmp/a → /u/u1/.sessions/s1/tmp/a。
func ApplyAlias(abs string, aliases map[string]string) string {
	if len(aliases) == 0 {
		return abs
	}
	prefixes := make([]string, 0, len(aliases))
	for prefix := range aliases {
		prefixes = append(prefixes, prefix)
	}
	sort.Slice(prefixes, func(i, j int) bool { return len(prefixes[i]) > len(prefixes[j]) })
	for _, prefix := range prefixes {
		target := aliases[prefix]
		if abs == prefix {
			return target
		}
		if strings.HasPrefix(abs, prefix+"/") {
			return target + strings.TrimPrefix(abs, prefix)
		}
	}
	return abs
}
