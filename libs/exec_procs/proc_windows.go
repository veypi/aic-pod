//go:build windows

package exec_procs

import (
	"io"
	"os"
	"os/exec"
	"syscall"
)

// SetSysProcAttr 抑制子进程控制台窗口（供本包与 vcore/browser 直连起进程点共用）：
// GUI 进程（-H windowsgui）无控制台，Windows 会为 CUI 子进程（bash/git/node）
// 新分配一个控制台窗口（每执行一次命令闪一次黑框）——CREATE_NO_WINDOW 不为
// 子进程分配控制台（输出本就重定向到日志文件，无影响），HideWindow 双保险。
// 0x08000000 = CREATE_NO_WINDOW（stdlib syscall 不导出该常量，用字面量避免引入依赖）。
func SetSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x08000000,
	}
}

// newOutputWriter Windows 上经逐行转码（UTF-8 直写 / GBK 解码）后落盘，
// 解决中文系统 cmd/PowerShell 输出乱码（见 output.go 注释）。
func newOutputWriter(w io.Writer) io.Writer {
	return &windowsOutputWriter{w: w}
}

// killEntry 终止后台条目（§5.8：Windows 用 TerminateProcess；
// 托管任务 pid=0 无进程，仅 cancel 中止任务体）。
func killEntry(e *Entry) {
	if e.pid > 0 {
		if proc, err := os.FindProcess(e.pid); err == nil {
			_ = proc.Kill()
		}
	}
	if e.cancel != nil {
		e.cancel()
	}
}
