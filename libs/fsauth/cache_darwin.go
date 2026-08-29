//go:build darwin

package fsauth

import (
	"os"
	"path/filepath"
)

// cacheRootDirs（darwin）：常见工具链缓存目录候选（无存在性探测）。
// 进程内不变（home/环境变量常量），供 Policy 预计算 Decide 判定根集。
func cacheRootDirs() []string {
	var dirs []string
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs,
			filepath.Join(home, "Library", "Caches"),
			filepath.Join(home, ".npm"),
			filepath.Join(home, ".cargo"),
			filepath.Join(home, ".rustup"),
			filepath.Join(home, ".m2"),
			filepath.Join(home, ".gradle"),
			filepath.Join(home, ".bun"),
			filepath.Join(home, ".mix"),
			filepath.Join(home, ".cabal"),
			filepath.Join(home, ".local", "share", "pnpm"),
			filepath.Join(home, ".composer"),
			filepath.Join(home, "Library", "Developer", "Xcode", "DerivedData"),
		)
	}
	return append(dirs, os.Getenv("GOCACHE"), os.Getenv("XDG_CACHE_HOME"))
}

// CacheRoots（darwin）= cacheRootDirs 存在性过滤版：exec 沙箱 bind 白名单用
// （bind 源必须存在；每次实时探测，Start 频率低不缓存）。
// 单一事实源：fsauth 白名单与 exec_procs 沙箱 write bind 读同一份——
// exec_procs 经 CacheRoots() 引用，禁止另写。Decide 判定侧用 cacheRootDirs
// （无过滤，见 fsauth.go rebuildBaseRootsLocked）。
func CacheRoots() []string { return existingDirs(cacheRootDirs()...) }
