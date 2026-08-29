//go:build linux

package fsauth

import (
	"os"
	"path/filepath"
)

// cacheRootDirs（linux）：候选目录（无存在性探测；$XDG_CACHE_HOME 未设则
// ~/.cache，go-build/pip/pnpm/uv 均在其下）。供 Policy 预计算 Decide 判定根集。
// 单一事实源说明见 cache_darwin.go。
func cacheRootDirs() []string {
	var dirs []string
	if xdg := os.Getenv("XDG_CACHE_HOME"); xdg != "" {
		dirs = append(dirs, xdg)
	} else if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".cache"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs,
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
		)
	}
	return append(dirs, os.Getenv("GOCACHE"))
}

// CacheRoots（linux）= cacheRootDirs 存在性过滤版（exec 沙箱 bind 白名单用）。
// 单一事实源说明见 cache_darwin.go。
func CacheRoots() []string { return existingDirs(cacheRootDirs()...) }
