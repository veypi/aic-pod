//go:build !windows

// Package windpi 提供 Windows 进程级 DPI 感知设置（其它平台空实现）。
//
// Windows 的 DPI 继承规则：子进程（powershell/cmd 等）自身无 DPI manifest
// 或未调用 DPI API 时，继承父进程的 DPI 感知状态。父进程非 DPI 感知 →
// 系统对高 DPI 屏做虚拟化：GetSystemMetrics/CopyFromScreen/窗口 rect 等
// 屏幕像素 API 全部按逻辑分辨率返回（4K @150% 缩放下为 2560×1440），
// 导致全屏截图只截到逻辑区域、一切基于屏幕像素的指令（坐标换算/鼠标
// 定位/窗口尺寸）全部错位。
//
// aic-pod 客户端（cli/desktop）在 main() 开头调用 Enable()，使自身进入
// system 级 DPI 感知（物理像素），所有子进程自动继承——截图与像素类
// 指令按真实分辨率（如 3840×2160）运行。
package windpi

// Enable 设置进程级 DPI 感知。Windows 上调用 user32.SetProcessDPIAware()
// （system 级；manifest 已声明时幂等返回 FALSE，无副作用）；
// 其它平台为空操作。
func Enable() {}
