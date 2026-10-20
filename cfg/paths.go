package cfg

import (
	"os"
	"path/filepath"
	"strings"
)

// ExpandHome 展开路径的 ~ 前缀（~ 或 ~/xxx → 用户主目录）；无法取得主目录时
// 原样返回。配置保存与 grant fs 共用同一实现，避免两处展开语义漂移。
func ExpandHome(p string) string {
	if p == "~" {
		if h, err := os.UserHomeDir(); err == nil {
			return h
		}
		return p
	}
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, strings.TrimPrefix(p, "~/"))
		}
	}
	return p
}
