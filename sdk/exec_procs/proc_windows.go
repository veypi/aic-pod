//go:build windows

package exec_procs

import (
	"os"
	"os/exec"
)

func setSysProcAttr(cmd *exec.Cmd) {}

// killEntry 终止后台进程（§5.8：Windows 用 TerminateProcess）。
func killEntry(e *Entry) {
	if proc, err := os.FindProcess(e.pid); err == nil {
		_ = proc.Kill()
	}
	if e.cancel != nil {
		e.cancel()
	}
}
