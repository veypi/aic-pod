//go:build windows

package exec_procs

import (
	"io"
	"os"
	"os/exec"
)

func setSysProcAttr(cmd *exec.Cmd) {}

// newOutputWriter Windows 上经逐行转码（UTF-8 直写 / GBK 解码）后落盘，
// 解决中文系统 cmd/PowerShell 输出乱码（见 output.go 注释）。
func newOutputWriter(w io.Writer) io.Writer {
	return &windowsOutputWriter{w: w}
}

// killEntry 终止后台进程（§5.8：Windows 用 TerminateProcess）。
func killEntry(e *Entry) {
	if proc, err := os.FindProcess(e.pid); err == nil {
		_ = proc.Kill()
	}
	if e.cancel != nil {
		e.cancel()
	}
}
