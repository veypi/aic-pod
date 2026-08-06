package main

import (
	"fmt"
	"net/url"
	"strings"

	pod "github.com/veypi/aic-pod"
	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/vigo/logv"
	"github.com/wailsapp/wails/v3/pkg/application"
)

// App 是桌面壳：平台窗口管理（托盘/菜单/设置页导航）。
// host 会话生命周期在 api 包（Runner），配置读写走 cfg.Global——
// cli/desktop 共享同一份 config.yaml 与同一套 local_code 通道，desktop 只加 Wails 壳。
type App struct {
	platform application.Window // 平台窗口（托盘打开/聚焦用，单窗口）
}

// NewApp 创建桌面壳。
func NewApp() *App {
	return &App{}
}

// OpenPlatform 打开/聚焦平台窗口。
// 未绑定（无凭证）→ 直达设备管理页 {host}/hosts 引导绑定；已绑定 → 打开平台首页。
func (a *App) OpenPlatform(hostURL string) error {
	if strings.TrimSpace(hostURL) == "" {
		hostURL = cfg.Global.Host
	}
	if cfg.Global.Key == "" {
		// 未绑定 → 直达设备管理页 {host}/hosts
		o := *cfg.Global
		o.Host = hostURL
		hostURL = o.HostsURL()
	}
	app := application.Get()
	// 拼 local_code 参数：平台页面据此建立本地通道（local_handler.js 解析持久化，
	// 并经 get_status 验证桌面通道后应用桌面壳效果）；URL 已带 local_code（如
	// OpenPlatformURL 传入的 hosts 页）则不再追加。
	if cfg.Global.Port() != 0 && !strings.Contains(hostURL, "local_code=") {
		sep := "?"
		if strings.Contains(hostURL, "?") {
			sep = "&"
		}
		hostURL = hostURL + sep + "local_code=" + url.QueryEscape(pod.LocalCodeParam())
	}
	logv.Info().Msgf("opening platform: %s", hostURL)
	if w, ok := app.Window.Get("platform"); ok {
		w.SetURL(hostURL)
		w.Show()
		w.Focus()
		a.platform = w
		return nil
	}
	w := a.newPlatformWindow("AIC Platform", hostURL)
	if w == nil {
		return fmt.Errorf("create platform window failed")
	}
	a.platform = w
	return nil
}

// newPlatformWindow 创建平台主窗口（平台页/本机设置页共用同一窗口）。
// 直接解锁 wails runtime：页面是外部/本地 URL，fetch 不到 wails 内嵌 runtime，
// 不会自发 wails:runtime:ready → ExecJS 只入队永不执行（导航菜单等全部失效）。
// HandleMessage 走与页面桥相同的处理路径，三平台通用、幂等。
func (a *App) newPlatformWindow(title, url string) application.Window {
	w := application.Get().Window.NewWithOptions(application.WebviewWindowOptions{
		Name:   "platform",
		Title:  title,
		URL:    url,
		Width:  1280,
		Height: 800,
		// win 窗口挂全局应用菜单（mac 无效果，菜单天然全局）；不设则 win 无菜单栏
		UseApplicationMenu: true,
	})
	if w == nil {
		return nil
	}
	w.HandleMessage("wails:runtime:ready")
	return w
}

// OpenPlatformURL 应用内打开指定平台 URL（设置页「更新凭证」入口等）。
// URL 已由调用方拼好参数；窗口不存在时经 OpenPlatform 创建（其拼参逻辑对已含
// local_code 的 URL 不再追加）。
func (a *App) OpenPlatformURL(rawURL string) error {
	if a.platform == nil {
		return a.OpenPlatform(rawURL)
	}
	// 从本机设置页跳回平台时恢复窗口标题
	a.platform.SetTitle("AIC Platform")
	a.platform.SetURL(rawURL)
	a.platform.Show()
	a.platform.Focus()
	return nil
}

// PlatformWindow 返回平台窗口（常驻托盘模式：关闭=隐藏，托盘点击重新打开）。
func (a *App) PlatformWindow() application.Window {
	return a.platform
}

// FocusPlatform 显示并聚焦平台窗口（菜单「打开平台」用；窗口不存在时创建）。
func (a *App) FocusPlatform() {
	if a.platform != nil {
		a.platform.Show()
		a.platform.Focus()
		return
	}
	if err := a.OpenPlatform(""); err != nil {
		logv.Warn().Msgf("focus platform failed: %v", err)
	}
}

// Navigate 原生菜单导航到平台路由（header 隐藏后的页面入口）。
// 优先 SPA 内跳转（毫秒级，不整页重载）：wails3 的 ExecJS 有 runtimeLoaded 门控——
// 平台页面经 external 桥发 'wails:runtime:ready'（env.js desktop 分支）后立即执行；
// pushState+popstate 是标准浏览器事件，vhtml 路由监听 popstate（router.js）。
// ExecJS 不可用（页面未发 ready 等）或跨域时回退 location.href 整页加载。
func (a *App) Navigate(route string) {
	target := strings.TrimSuffix(cfg.Global.Host, "/") + route
	if cfg.Global.Key == "" {
		// 未绑定：忽略路由，由 OpenPlatform 统一引导到设备管理页
		target = cfg.Global.Host
	}
	if a.platform == nil {
		if err := a.OpenPlatform(target); err != nil {
			logv.Warn().Msgf("navigate %s failed: %v", route, err)
		}
		return
	}
	js := fmt.Sprintf(`(function(){try{history.pushState({},'',%q);dispatchEvent(new PopStateEvent('popstate'));}catch(e){location.href=%q;}})()`, target, target)
	a.platform.ExecJS(js)
	a.platform.Show()
	a.platform.Focus()
}

// openSettings 打开/聚焦设置窗口（本地页面，配置 host/key/work_dir/exec_timeout）。
func (a *App) openSettings() {
	if cfg.Global.Port() == 0 {
		return
	}
	// 主窗口打开本机设置页（api 包 serve 的 127.0.0.1 页面）：不依赖平台可达，
	// 单窗口统一——设置 = 主窗口导航到本地地址，「返回平台」导航回平台。
	// code 经 URL query 传入页面 JS（x-aic-code 头的来源）。
	url := fmt.Sprintf("http://127.0.0.1:%d/settings?code=%s", cfg.Global.Port(), cfg.Global.Code())
	if a.platform == nil {
		w := a.newPlatformWindow("AIC Desktop 设置", url)
		if w == nil {
			return
		}
		a.platform = w
		return
	}
	a.platform.SetTitle("AIC Desktop 设置")
	a.platform.SetURL(url)
	a.platform.Show()
	a.platform.Focus()
}

// 本机设置页已下沉到 api 包（/settings 提供，静态资源 api/ui/settings.html，
// 主窗口与浏览器共用）。
