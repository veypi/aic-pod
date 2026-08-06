package main

import (
	"fmt"
	"runtime"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// 原生菜单（mac 菜单栏 / windows 窗口菜单栏；linux 的 SetApplicationMenu 为 no-op，自动降级）。
//
// 关键坑：wails3 beta.3 的编辑菜单构造函数（NewUndoMenuItem/NewCutMenuItem 等）在
// darwin 上未 SetRole——无角色则 menuitem_darwin 生成 handleClick 项而非原生 selector
// （responder chain），且构造函数只在非 darwin 注册 OnClick → mac 上 Cmd+C/V/Z 完全无效。
// 因此 darwin 上必须为编辑项显式补角色（menuItem 助手的第三个参数）。

// menuItem 包装 wails 菜单项构造器：中文标签 + darwin 补原生角色（role 传 NoRole 则不补，
// 保留构造函数自带的 OnClick 跨平台行为）。
func menuItem(ctor func() *application.MenuItem, label string, role application.Role) *application.MenuItem {
	it := ctor().SetLabel(label)
	if runtime.GOOS == "darwin" && role != application.NoRole {
		it.SetRole(role)
	}
	return it
}

// addItems 向菜单追加一组已构造的菜单项（Menu 无 AddItem 公开 API，经 NewMenuFromItems+Append）。
func addItems(m *application.Menu, items ...*application.MenuItem) {
	if len(items) == 0 {
		return
	}
	m.Append(application.NewMenuFromItems(items[0], items[1:]...))
}

// buildAppMenu 构建应用菜单。svc 用于自定义动作（设置窗口 / 聚焦平台窗口）。
func buildAppMenu(svc *App) *application.Menu {
	menu := application.NewMenu()

	settings := func() *application.MenuItem {
		return application.NewMenuItem("设置…").
			SetAccelerator("CmdOrCtrl+,").
			OnClick(func(*application.Context) { svc.openSettings() })
	}

	if runtime.GOOS == "darwin" {
		// 应用菜单（mac 菜单栏惯例）
		appm := menu.AddSubmenu("AIC Desktop")
		addItems(appm,
			menuItem(application.NewAboutMenuItem, "关于 AIC Desktop", application.NoRole),
			application.NewMenuItemSeparator(),
			settings(),
			application.NewMenuItemSeparator(),
			menuItem(application.NewHideMenuItem, "隐藏 AIC Desktop", application.Hide),
			menuItem(application.NewHideOthersMenuItem, "隐藏其它", application.HideOthers),
			menuItem(application.NewUnhideMenuItem, "全部显示", application.ShowAll),
			application.NewMenuItemSeparator(),
			menuItem(application.NewQuitMenuItem, "退出 AIC Desktop", application.NoRole),
		)
	} else {
		// 文件菜单（win/linux 惯例：设置与退出的入口）
		filem := menu.AddSubmenu("文件")
		addItems(filem,
			settings(),
			application.NewMenuItemSeparator(),
			menuItem(application.NewQuitMenuItem, "退出", application.NoRole),
		)
	}

	// 导航：平台主导航入口（desktop 模式 header 隐藏后的页面入口，见 env.js
	// aic-shell-desktop）。与 ui/layout/header.html 的 menus 保持一致；/admin 不在
	// 此列——壳层无法判断权限，平台路由守卫自带权限控制。
	nav := menu.AddSubmenu("导航")
	navItems := []struct {
		label string
		route string
	}{
		{"控制台", "/settings/"},
		{"Agents", "/a"},
		{"设备", "/hosts"},
		{"Skills", "/skills"},
		{"Voices", "/voices"},
		{"AgentOS", "/os"},
	}
	items := make([]*application.MenuItem, 0, len(navItems))
	for i, n := range navItems {
		route := n.route
		items = append(items, application.NewMenuItem(n.label).
			SetAccelerator(fmt.Sprintf("CmdOrCtrl+%d", i+1)).
			OnClick(func(*application.Context) { svc.Navigate(route) }))
	}
	addItems(nav, items...)

	// 编辑：darwin 必须补角色（见文件头注释），webview 内经 responder chain 原生响应
	edit := menu.AddSubmenu("编辑")
	addItems(edit,
		menuItem(application.NewUndoMenuItem, "撤销", application.Undo),
		menuItem(application.NewRedoMenuItem, "重做", application.Redo),
		application.NewMenuItemSeparator(),
		menuItem(application.NewCutMenuItem, "剪切", application.Cut),
		menuItem(application.NewCopyMenuItem, "复制", application.Copy),
		menuItem(application.NewPasteMenuItem, "粘贴", application.Paste),
		menuItem(application.NewSelectAllMenuItem, "全选", application.SelectAll),
	)

	// 视图：构造函数自带全平台 OnClick（Reload/Zoom/Fullscreen 无 darwin selector 映射）
	view := menu.AddSubmenu("视图")
	addItems(view,
		menuItem(application.NewReloadMenuItem, "重新加载", application.NoRole),
		menuItem(application.NewForceReloadMenuItem, "强制刷新", application.NoRole),
		application.NewMenuItem("开发者工具").
			SetAccelerator("Alt+CmdOrCtrl+I").
			OnClick(func(*application.Context) {
				if w := application.Get().Window.Current(); w != nil {
					w.OpenDevTools()
				}
			}),
		application.NewMenuItemSeparator(),
		menuItem(application.NewZoomResetMenuItem, "实际大小", application.NoRole),
		menuItem(application.NewZoomInMenuItem, "放大", application.NoRole),
		menuItem(application.NewZoomOutMenuItem, "缩小", application.NoRole),
		application.NewMenuItemSeparator(),
		menuItem(application.NewToggleFullscreenMenuItem, "切换全屏", application.NoRole),
	)

	// 窗口：关闭触发现有 WindowClosing hook（= 隐藏窗口，常驻托盘语义不变）
	winm := menu.AddSubmenu("窗口")
	addItems(winm,
		menuItem(application.NewMinimiseMenuItem, "最小化", application.NoRole),
		menuItem(application.NewCloseMenuItem, "关闭窗口", application.NoRole),
		application.NewMenuItemSeparator(),
		application.NewMenuItem("打开平台").OnClick(func(*application.Context) {
			svc.FocusPlatform()
		}),
	)

	return menu
}
