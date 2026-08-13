//go:build linux

package exec_procs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/veypi/aic-pod/libs/proto"
)

// probeBackend（linux）：bwrap 功能性探测——真跑一次最小只读 profile，
// exit 0 = 内核接受并强制（bwrap 的 mount profile 按构造即 full enforcement）。
func probeBackend() sandboxBackend {
	if _, err := exec.LookPath("bwrap"); err != nil {
		return backendUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bwrap",
		"--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc", "--die-with-parent", "--", "true")
	if cmd.Run() != nil {
		return backendUnavailable
	}
	return backendBwrap
}

// planConfined（linux）：bwrap argv 包装，无令牌。
// 可写根下的敏感子路径（.git 等）存在时收集为只读覆盖；git 自身豁免
// （保护对象是 bash/rm 等通用命令，git 等级由 vcore 子命令表承担）。
func planConfined(level int, workdir string, argv []string) (launchPlan, error) {
	if selectBackend() == backendUnavailable {
		return launchPlan{}, sandboxUnavailable(level)
	}
	var protected []string
	if level >= proto.LevelWrite && workdir != "" && !isGitArgv(argv) {
		for _, name := range protectedMetadataNames {
			p := filepath.Join(workdir, name)
			if st, err := os.Stat(p); err == nil && st.IsDir() {
				protected = append(protected, p)
			}
		}
	}
	return launchPlan{argv: bwrapArgs(level, workdir, cacheRoots(), protected, argv)}, nil
}

// cacheRoots（linux）：$XDG_CACHE_HOME（未设则 ~/.cache，go-build/pip/pnpm/uv
// 均在其下）+ ~/.npm。$GOCACHE 显式设置时并入。存在性过滤。
func cacheRoots() []string {
	var dirs []string
	if xdg := os.Getenv("XDG_CACHE_HOME"); xdg != "" {
		dirs = append(dirs, xdg)
	} else if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".cache"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".npm"))
	}
	dirs = append(dirs, os.Getenv("GOCACHE"))
	return existingDirs(dirs...)
}
