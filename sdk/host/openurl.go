package host

import (
	"fmt"
	"os/exec"
	"runtime"
)

// OpenExternal 用系统默认浏览器打开 URL（desktop 外链委托，2026-08-06）。
// 纯标准库实现（cli/desktop 共用，无 wails 依赖）：
//   darwin:  open <url>
//   windows: rundll32 url.dll,FileProtocolHandler <url>
//   linux:   xdg-open <url>
//
// 安全：argv 数组直传（不经 shell 解释），URL 作为单参数；调用方（LocalAPI
// handleOpenURL）已校验 scheme 仅 http/https。cmd.Start 后不等待——等待会让
// 请求挂起直到浏览器退出，异步 Wait 回收进程即可。
func OpenExternal(rawurl string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", rawurl)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", rawurl)
	default:
		cmd = exec.Command("xdg-open", rawurl)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("open external url: %w", err)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
