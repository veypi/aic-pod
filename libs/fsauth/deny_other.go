//go:build !darwin && !linux && !windows

package fsauth

// defaultDenyPaths（其他平台）：仅通用条目（无平台特有路径知识）。
func defaultDenyPaths() []string {
	return denyCommon()
}
