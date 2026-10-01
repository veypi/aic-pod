//go:build windows

package chrome

import (
	"fmt"
	"golang.org/x/sys/windows"
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// Chromium accepts inheritable HANDLE values via --remote-debugging-io-pipes.
// https://chromium.googlesource.com/chromium/src/+/main/content/browser/devtools/devtools_agent_host_impl.cc
func configure(cmd *exec.Cmd, read, write *os.File) error {
	handles := []syscall.Handle{syscall.Handle(read.Fd()), syscall.Handle(write.Fd())}
	for _, h := range handles {
		if err := windows.SetHandleInformation(windows.Handle(h), windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
			return err
		}
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, AdditionalInheritedHandles: handles}
	cmd.Args = append(cmd.Args, fmt.Sprintf("--remote-debugging-io-pipes=%d,%d", uint32(read.Fd()), uint32(write.Fd())))
	return nil
}
func kill(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = exec.Command("taskkill", "/PID", strconv.Itoa(cmd.Process.Pid), "/T", "/F").Run()
		_ = cmd.Process.Kill()
	}
}
