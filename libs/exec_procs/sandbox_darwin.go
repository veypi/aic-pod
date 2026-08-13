//go:build darwin

package exec_procs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
)

// probeBackend（darwin）：sandbox-exec（Seatbelt）功能性探测——真跑一次
// read-only profile，exit 0 = profile 被内核接受并强制。固定 /usr/bin 路径
// （防 PATH 注入）；Apple 标记该 CLI deprecated 但仍随系统提供，若未来
// 移除，此探测即 fail-closed。
func probeBackend() sandboxBackend {
	if _, err := os.Stat(macosSeatbeltExecutable); err != nil {
		return backendUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, macosSeatbeltExecutable,
		"-p", "(version 1)(allow default)(deny file-write*)(allow file-write* (literal \"/dev/null\"))",
		"--", "true")
	if cmd.Run() != nil {
		return backendUnavailable
	}
	return backendSeatbelt
}

// planConfined（darwin）：sandbox-exec argv 包装，无令牌。
func planConfined(level int, workdir string, argv []string) (launchPlan, error) {
	if selectBackend() == backendUnavailable {
		return launchPlan{}, sandboxUnavailable(level)
	}
	return launchPlan{argv: seatbeltArgs(level, workdir, argv)}, nil
}

// cacheRoots（darwin）：常见工具链缓存目录（go-build/pip/pnpm/uv/Homebrew 均在
// ~/Library/Caches 下；npm 另用 ~/.npm）。$GOCACHE/$XDG_CACHE_HOME 显式设置时并入。
// 存在性过滤（不存在的目录不产出）。
func cacheRoots() []string {
	var dirs []string
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, "Library", "Caches"), filepath.Join(home, ".npm"))
	}
	dirs = append(dirs, os.Getenv("GOCACHE"), os.Getenv("XDG_CACHE_HOME"))
	return existingDirs(dirs...)
}
