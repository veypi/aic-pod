//go:build !windows

package host

import (
	"os/exec"
	"syscall"
	"time"
)

func setSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killBackground 终止后台进程：先对整个进程组发 SIGTERM，5s 未退出补 SIGKILL（§5.8）。
func killBackground(p *bgProcess) {
	// 进程组杀死（Setpgid 使 pgid == pid）
	_ = syscall.Kill(-p.pid, syscall.SIGTERM)
	go func() {
		timer := time.NewTimer(5 * time.Second)
		<-timer.C
		select {
		case <-p.done:
		default:
			_ = syscall.Kill(-p.pid, syscall.SIGKILL)
		}
	}()
	// cancel 兜底（CommandContext 会 Kill 进程）
	if p.cancel != nil {
		p.cancel()
	}
}
