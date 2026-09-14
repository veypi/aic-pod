//
// sysca.go
// Copyright (C) 2026 veypi <i@veypi.com>
// Distributed under terms of the MIT license.
//

package fsauth

import (
	"path/filepath"
)

// SystemCAReadPatterns 返回系统公共 CA 的只读放行模式（exec 沙箱专用）。
//
// 背景（2026-09-11 实测）：deny 通用表 **/*.pem 的意图是保护私钥/凭证，
// 但同样命中系统公共 CA bundle（/etc/ssl/cert.pem 等）——沙箱内 curl/node
// 等因无法加载证书而 https 全断（stat/读被拒，curl error 77；网络层本身
// 完全正常）。同类影响一切依赖系统 CA 的 CLI（wget、系统 git-https 等）。
// 系统 CA 是公开信任锚（非机密），给予只读豁免。
//
// 仅只读用途：调用方只许输出 file-read* 放行——**绝不放行写**
// （写系统 CA 目录 = 自定义信任根注入）。
//
// 输出与 deny 同口径展开：canonicalPattern（符号链接展开，如 darwin
// /etc → /private/etc）+ 字面形态，保证 seatbelt 规范化路径两态命中。
func SystemCAReadPatterns() []string {
	pats := systemCAPaths()
	out := make([]string, 0, len(pats)*2)
	seen := map[string]bool{}
	add := func(s string) {
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	for _, pat := range pats {
		e, ok := expandVars(pat)
		if !ok {
			continue
		}
		add(canonicalPattern(e))
		add(filepath.ToSlash(e))
	}
	return out
}
