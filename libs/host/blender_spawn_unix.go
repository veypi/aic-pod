//go:build !windows

package host

import (
	"os/exec"
	"syscall"
)

// spawnDetached 拉起独立 GUI 进程（setsid 脱离会话，不随 pod 退出被 SIGHUP）。
func spawnDetached(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd
}
