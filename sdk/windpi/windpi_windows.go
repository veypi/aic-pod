//go:build windows

package windpi

import "syscall"

var (
	user32                 = syscall.NewLazyDLL("user32.dll")
	procSetProcessDPIAware = user32.NewProc("SetProcessDPIAware")
)

// Enable 调用 SetProcessDPIAware 使本进程进入 system 级 DPI 感知：
// 此后 GetSystemMetrics/CopyFromScreen 等返回物理像素（如 4K 3840×2160），
// 且所有子进程（powershell/cmd/...，自身无 DPI manifest 时）继承该状态。
// 必须在任何子进程/GDI 对象创建之前调用；重复调用或已由 manifest 声明时
// 返回 FALSE，属正常，忽略返回值。
func Enable() {
	procSetProcessDPIAware.Call()
}
