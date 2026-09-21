//go:build windows

package fsauth

import (
	"os"
	"path/filepath"
)

// cacheRootDirs（windows）：候选目录（无存在性探测；精确子目录——不放行整个
// %LOCALAPPDATA%，其下含大量应用数据/凭证存储）。供 Policy 预计算 Decide 判定根集。
// 单一事实源说明见 cache_darwin.go。
func cacheRootDirs() []string {
	var dirs []string
	if lad := os.Getenv("LOCALAPPDATA"); lad != "" {
		dirs = append(dirs,
			filepath.Join(lad, "go-build"),
			filepath.Join(lad, "npm-cache"),
			filepath.Join(lad, "pip", "Cache"),
			filepath.Join(lad, "pnpm"),
			filepath.Join(lad, "deno"),
			filepath.Join(lad, "Yarn", "Cache"),
			filepath.Join(lad, "uv", "cache"),
		)
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
	return appendEnvDirs(dirs, "GOCACHE", "XDG_CACHE_HOME")
}

// CacheRoots（windows）= cacheRootDirs 存在性过滤版（exec 沙箱 bind 白名单用）。
// 单一事实源说明见 cache_darwin.go。
func CacheRoots() []string { return existingDirs(cacheRootDirs()...) }
