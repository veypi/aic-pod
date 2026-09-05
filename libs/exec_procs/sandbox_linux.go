//go:build linux

package exec_procs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/veypi/aic-pod/libs/fsauth"
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
func planConfined(level int, workdir string, extra []string, argv []string, deny []string) (launchPlan, error) {
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
	// 可写 bind 列表：工具链缓存（fsauth.CacheRoots）+ 公共区 $HOME/.aic（publicRoots）
	// + 追加根（fsauth 配置白名单/临时 grant，v0.14.5 统一名单）
	cacheDirs := append(fsauth.CacheRoots(), publicRoots()...)
	cacheDirs = append(cacheDirs, extra...)
	return launchPlan{argv: bwrapArgs(level, workdir, cacheDirs, protected, argv, deny)}, nil
}
