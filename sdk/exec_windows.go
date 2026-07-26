//go:build windows

package aichost

import (
	"os"
	"os/exec"
	"time"
)

func setSysProcAttr(cmd *exec.Cmd) {}

// killBackground 终止后台进程（Windows：TerminateProcess，§6.2）。
func killBackground(p *bgProcess) {
	// cancel 触发 CommandContext 的 Process.Kill（即 TerminateProcess）
	if p.cancel != nil {
		p.cancel()
	}
	// 兜底：cancel 未生效时直接 Kill
	go func() {
		time.Sleep(2 * time.Second)
		bgRegistryMu.Lock()
		done := p.done
		bgRegistryMu.Unlock()
		if !done {
			if proc, err := os.FindProcess(p.pid); err == nil {
				_ = proc.Kill()
			}
		}
	}()
}
