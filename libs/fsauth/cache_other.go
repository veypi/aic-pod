//go:build !darwin && !linux && !windows

package fsauth

// 其他平台：无已知工具链缓存约定，候选与过滤版均空表（白名单宁缺毋滥）。
func cacheRootDirs() []string { return nil }

// CacheRoots（其他平台）：见 cache_darwin.go（exec 沙箱 bind 白名单用）。
func CacheRoots() []string { return nil }
