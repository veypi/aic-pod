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
	return appendEnvDirs(dirs, "GOCACHE", "XDG_CACHE_HOME")
}
