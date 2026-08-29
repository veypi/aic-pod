package fsauth

import (
	"path/filepath"
	"runtime"
	"strings"
)

// matchPattern 判定 canonical 路径是否命中 glob 模式（v0.14.5 §2 匹配器）：
//   - 支持跨段 `**`（匹配零或多段）；段内 `*`（任意非分隔符字符）、`?`（单字符）；
//     `[` 按字面匹配（不支持字符类；与 canonicalPattern 的字面前缀检测集同口径）；
//   - 斜杠语义：模式与路径统一 filepath.ToSlash（windows 反斜杠归一）；
//   - 大小写按平台折叠：darwin/windows 文件系统默认不敏感 → fold，linux 敏感；
//   - 与 rg 的 glob 语义独立实现、不共代码（rg 明确不支持 `**`——两者勿混用文档）。
func matchPattern(pattern, cpath string) bool {
	if pattern == "" {
		return false
	}
	pattern = filepath.ToSlash(pattern)
	cpath = filepath.ToSlash(cpath)
	if foldCase {
		pattern = strings.ToLower(pattern)
		cpath = strings.ToLower(cpath)
	}
	psegs := strings.Split(strings.TrimPrefix(pattern, "/"), "/")
	csegs := strings.Split(strings.TrimPrefix(cpath, "/"), "/")
	return matchSegs(psegs, csegs)
}

// foldCase 平台大小写折叠（win/mac 文件系统默认不敏感）。
const foldCase = runtime.GOOS == "windows" || runtime.GOOS == "darwin"

// matchSegs 递归匹配分段序列：** 消耗零或多段，其余段内模式匹配。
func matchSegs(psegs, csegs []string) bool {
	if len(psegs) == 0 {
		return len(csegs) == 0
	}
	if psegs[0] == "**" {
		// ** 后无剩余 = 匹配一切剩余
		if len(psegs) == 1 {
			return true
		}
		for i := 0; i <= len(csegs); i++ {
			if matchSegs(psegs[1:], csegs[i:]) {
				return true
			}
		}
		return false
	}
	if len(csegs) == 0 {
		return false
	}
	if !matchOne(psegs[0], csegs[0]) {
		return false
	}
	return matchSegs(psegs[1:], csegs[1:])
}

// matchOne 单段匹配：* = 任意字符序列（不含 /），? = 单字符，其余字面。
func matchOne(pat, s string) bool {
	px, sx := 0, 0
	star, starX := -1, 0
	for sx < len(s) {
		if px < len(pat) && (pat[px] == '?' || pat[px] == s[sx]) {
			px++
			sx++
			continue
		}
		if px < len(pat) && pat[px] == '*' {
			star = px
			starX = sx
			px++
			continue
		}
		if star >= 0 {
			px = star + 1
			starX++
			sx = starX
			continue
		}
		return false
	}
	for px < len(pat) && pat[px] == '*' {
		px++
	}
	return px == len(pat)
}
