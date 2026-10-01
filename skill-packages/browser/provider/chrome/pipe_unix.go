//go:build !windows

package chrome

import (
	"os"
	"os/exec"
	"syscall"
)

func configure(cmd *exec.Cmd, read, write *os.File) error {
	cmd.ExtraFiles = []*os.File{read, write}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return nil
}
func kill(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
