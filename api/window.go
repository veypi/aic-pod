package api

import (
	"github.com/veypi/vigo"
)

// 窗口控制（desktop 专用）：经 WindowControl 变量注入实现——desktop main 挂
// wails window API（api 包不依赖 wails，desktop 是独立 module，同 OpenPlatformURL
// 模式）；cli 无窗口 → 返回 Desktop:false（壳页面据此隐藏窗口按钮区）。
//
// 壳页面（本地 vhtml）窗口按钮 → fetch /api/window_*（同源，x-aic-code 头）。

// WindowState 是窗口状态视图（window_state 返回；动作端点亦回显最新状态）。
type WindowState struct {
	Desktop    bool `json:"desktop"`
	Maximised  bool `json:"maximised"`
	Fullscreen bool `json:"fullscreen"`
}

// WindowControl 是窗口控制实现（desktop main 注入；nil = 非桌面环境）。
// action: window_state | window_minimise | window_maximise | window_close |
// window_fullscreen | window_pet | window_restore
// args（仅 window_pet）：鼠标屏幕坐标 {x, y}——桌宠出现在鼠标下方。
var WindowControl func(action string, args ...int) WindowState

func windowState(action string, args ...int) *WindowState {
	if WindowControl == nil {
		return &WindowState{Desktop: false}
	}
	s := WindowControl(action, args...)
	return &s
}

// GetWindowState 返回窗口状态与是否桌面环境（壳页面探测用）。
func GetWindowState(x *vigo.X) (*WindowState, error) {
	return windowState("window_state"), nil
}

// WindowMinimise 最小化窗口。
func WindowMinimise(x *vigo.X) (*WindowState, error) {
	return windowState("window_minimise"), nil
}

// WindowMaximise 最大化/还原（toggle）。
func WindowMaximise(x *vigo.X) (*WindowState, error) {
	return windowState("window_maximise"), nil
}

// WindowClose 关闭窗口（= 隐藏，托盘常驻语义）。
func WindowClose(x *vigo.X) (*WindowState, error) {
	return windowState("window_close"), nil
}

// WindowFullscreen 全屏/退出（toggle）。
func WindowFullscreen(x *vigo.X) (*WindowState, error) {
	return windowState("window_fullscreen"), nil
}

// PetReq 是 window_pet 的入参：鼠标屏幕坐标（桌宠出现在鼠标下方）。
type PetReq struct {
	X int `json:"x"`
	Y int `json:"y"`
}

// WindowPet 缩成桌宠：窗口缩至最小尺寸，出现在鼠标下方；前端切 /pet 页。
func WindowPet(x *vigo.X, req *PetReq) (*WindowState, error) {
	if req == nil {
		return windowState("window_pet"), nil
	}
	return windowState("window_pet", req.X, req.Y), nil
}

// WindowRestore 桌宠恢复：还原窗口尺寸，前端切回首页。
func WindowRestore(x *vigo.X) (*WindowState, error) {
	return windowState("window_restore"), nil
}
