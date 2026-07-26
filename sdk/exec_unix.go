//go:build !windows

package aichost

import (
	"os/exec"
	"syscall"
	"time"
)

func setSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killBackground 终止后台进程：先对整个进程组发 SIGTERM，5s 未退出补 SIGKILL（§6.2）。
func killBackground(p *bgProcess) {
	// 进程组杀死（Setpgid 使 pgid == pid）
	_ = syscall.Kill(-p.pid, syscall.SIGTERM)
	go func() {
		time.Sleep(5 * time.Second)
		bgRegistryMu.Lock()
		done := p.done
		bgRegistryMu.Unlock()
		if !done {
			_ = syscall.Kill(-p.pid, syscall.SIGKILL)
		}
	}()
	// cancel 兜底（CommandContext 会 Kill 进程）
	if p.cancel != nil {
		p.cancel()
	}
}
