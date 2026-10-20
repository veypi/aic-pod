package execution

// cloudpath.go 是 cloud 用户路径的单一解析规则（exec 适配器与 aic fs 工具共用）：
// 相对路径以用户根展开 → 别名重定向（/tmp → 会话空间）→ jail 硬边界。
// host 不启用本策略（host 的边界由规则表表达）。

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/veypi/aic-pod/protocol"
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

// CloudPathPolicy 解析 cloud 调用路径：Root 是用户根（相对路径基准与 jail 首根），
// Aliases 是路径别名，Jail 是额外可达根（如 /skills、/public）。
type CloudPathPolicy struct {
	Root    string
	Aliases map[string]string
	Jail    []string
}

// Resolve 返回 FS 内绝对逻辑路径；越出 jail 返回可读错误（含原始输入）。
func (p CloudPathPolicy) Resolve(name string) (string, error) {
	orig := name
	if !strings.HasPrefix(name, "/") {
		name = path.Join(p.Root, name)
	}
	abs := ApplyAlias(path.Clean(name), p.Aliases)
	roots := append([]string{p.Root}, p.Jail...)
	if !protocol.WithinRoots(abs, roots) {
		note := strings.Join(roots, "、")
		if orig != abs {
			return "", fmt.Errorf("%s → %s 越出 jail（cloud 文件访问限定在 %s）", orig, abs, note)
		}
		return "", fmt.Errorf("%s 越出 jail（cloud 文件访问限定在 %s）", abs, note)
	}
	return abs, nil
}
