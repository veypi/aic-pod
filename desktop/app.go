package main

import (
	"fmt"
	"net/url"

	"github.com/veypi/aic-pod/api"
	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/vigo/logv"
	"github.com/wailsapp/wails/v3/pkg/application"
)

// App 是桌面壳：frameless 窗口加载本地壳页面（vigo serve，header + iframe 平台页）。
// 平台导航/窗口按钮全部由壳页面承担（/api/window_*），wails 只提供窗口容器；
// host 会话生命周期在 api 包（Runner），配置读写走 cfg.Global——
// cli/desktop 共享同一份 config.yaml。
type App struct {
	platform application.Window // 平台窗口（托盘打开/聚焦用，单窗口）
}

// NewApp 创建桌面壳。
func NewApp() *App {
	return &App{}
}

// OpenShell 创建/聚焦壳窗口：URL = 本地壳页面（首次携带 code 秘钥，
// 壳页面 env.js 存入 localStorage 并在 $fetch 自动注入 x-aic-code）。
func (a *App) OpenShell() error {
	if w, ok := application.Get().Window.Get("platform"); ok {
		w.Show()
		w.Focus()
		a.platform = w
		return nil
	}
	w := application.Get().Window.NewWithOptions(application.WebviewWindowOptions{
		Name:   "platform",
		Title:  "AIC Desktop",
		Width:  1280,
		Height: 800,
		MinWidth:  800,
		MinHeight: 600,
		// 本地壳页面：header（菜单+窗口按钮）+ iframe 平台页，由 vigo serve
		URL: fmt.Sprintf("http://127.0.0.1:%d/?code=%s",
			cfg.Global.Port(), url.QueryEscape(cfg.Global.Code)),
		// 无系统边框/按钮：自定义标题栏（壳 header 拖动区 + 窗口按钮）
		Frameless:             true,
		UseApplicationMenu:    false,
		MinimiseButtonState:   application.ButtonHidden,
		MaximiseButtonState:   application.ButtonHidden,
		CloseButtonState:      application.ButtonHidden,
		FullscreenButtonState: application.ButtonHidden,
		// 透明窗口：mac 认 Mac.Backdrop（window+webview 双透明），正常页面有
		// 背景色不受影响，桌宠页（/pet）透明露出桌面
		BackgroundType: application.BackgroundTypeTransparent,
		// mac：内容延伸到标题栏（自定义 header 全高）+ 透明 backdrop
		Mac: application.MacWindow{
			Backdrop: application.MacBackdropTransparent,
			TitleBar: application.MacTitleBar{
				AppearsTransparent: true,
				Hide:               true,
				HideTitle:          true,
				FullSizeContent:    true,
			},
		},
	})
	if w == nil {
		return fmt.Errorf("create shell window failed")
	}
	a.platform = w
	return nil
}

// FocusPlatform 显示并聚焦壳窗口（托盘/单实例第二实例用；窗口不存在时创建）。
func (a *App) FocusPlatform() {
	if a.platform == nil {
		if err := a.OpenShell(); err != nil {
			logv.Warn().Msgf("open shell failed: %v", err)
		}
		return
	}
	a.platform.Show()
	a.platform.Focus()
}

// PlatformWindow 返回壳窗口（常驻托盘模式：关闭=隐藏，托盘点击重新打开）。
func (a *App) PlatformWindow() application.Window {
	return a.platform
}

// 桌宠切换的位置/尺寸缓存：非桌宠记住自己的位置，反复切换互不干扰；
// 桌宠不缓存——每次进入按鼠标位置出现。
var (
	petOrigW, petOrigH int // 非桌宠尺寸（window_pet 时记录，window_restore 还原）
	normalPosX, normalPosY int // 非桌宠位置缓存（进桌宠时记录，恢复时回去）
)

// 桌宠尺寸（window_pet 时缩至该大小）。
const petSize = 200

// windowControl 实现 api.WindowControl（壳页面 /api/window_* 的窗口动作）：
// minimse/maximise(还原)/close(=隐藏，托盘常驻)/fullscreen(toggle)/pet(缩成桌宠，
// 按鼠标坐标出现在鼠标下方)/restore(桌宠还原)，并回显状态。
func windowControl(action string, args ...int) api.WindowState {
	st := api.WindowState{Desktop: true}
	w, ok := application.Get().Window.Get("platform")
	if !ok || w == nil {
		return st
	}
	switch action {
	case "window_minimise":
		w.Minimise()
	case "window_maximise":
		w.ToggleMaximise()
	case "window_close":
		w.Hide()
	case "window_fullscreen":
		w.ToggleFullscreen()
	case "window_pet":
		// 记录非桌宠尺寸/位置 → 缩至桌宠大小；位置 = 鼠标下方（前端传屏幕坐标，
		// wails 坐标同为左上原点 Y-down，语义一致）
		ow, oh := w.Size()
		petOrigW, petOrigH = ow, oh
		normalPosX, normalPosY = w.RelativePosition()
		w.SetSize(petSize, petSize)
		if len(args) >= 2 {
			// 鼠标在宠物中心：窗口以鼠标坐标为中心定位
			w.SetRelativePosition(args[0]-petSize/2, args[1]-petSize/2)
		} else {
			w.Center()
		}
	case "window_restore":
		// 还原尺寸，位置回非桌宠缓存
		if petOrigW > 0 && petOrigH > 0 {
			w.SetSize(petOrigW, petOrigH)
		}
		w.SetRelativePosition(normalPosX, normalPosY)
	}
	st.Maximised = w.IsMaximised()
	st.Fullscreen = w.IsFullscreen()
	return st
}
