//go:build !darwin && !linux && !windows

package fsauth

// 其他平台：无已知工具链缓存约定，候选与过滤版均空表（白名单宁缺毋滥）。
func cacheRootDirs() []string { return nil }
