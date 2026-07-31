//go:build windows

package host

import (
	"os"
	"os/exec"
)

func setSysProcAttr(cmd *exec.Cmd) {}

// killBackground 终止后台进程（§5.8：Windows 用 TerminateProcess）。
func killBackground(p *bgProcess) {
	if proc, err := os.FindProcess(p.pid); err == nil {
		_ = proc.Kill()
	}
	if p.cancel != nil {
		p.cancel()
	}
}
