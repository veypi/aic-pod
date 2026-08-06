//go:build !windows

package exec_procs

import (
	"io"
	"os/exec"
	"syscall"
	"time"
)

// newOutputWriter 非 Windows 平台原样直写（子进程输出本就是 UTF-8）。
func newOutputWriter(w io.Writer) io.Writer { return w }

// SetSysProcAttr 设置子进程属性：独立进程组（killEntry 按进程组终止）。
func SetSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killEntry 终止后台进程：先对整个进程组发 SIGTERM，5s 未退出补 SIGKILL（§5.8）。
func killEntry(e *Entry) {
	// 进程组杀死（Setpgid 使 pgid == pid）
	_ = syscall.Kill(-e.pid, syscall.SIGTERM)
	go func() {
		timer := time.NewTimer(5 * time.Second)
		<-timer.C
		select {
		case <-e.done:
		default:
			_ = syscall.Kill(-e.pid, syscall.SIGKILL)
		}
	}()
	// cancel 兜底（CommandContext 会 Kill 进程）
	if e.cancel != nil {
		e.cancel()
	}
}
