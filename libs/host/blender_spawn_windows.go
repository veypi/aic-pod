//go:build windows

package host

import (
	"os/exec"
	"syscall"
)

// spawnDetached 拉起独立 GUI 进程（脱离控制台，不随 pod 退出）。
func spawnDetached(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000008 /* DETACHED_PROCESS */}
	return cmd
}
