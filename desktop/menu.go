package main

import (
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
		addItems(
			appm,
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
	}

	// 顶级路由（win/linux 从左到右：首页 / agents / os / 设置 / 菜单 / 视图；
	// darwin 前置应用菜单，编辑/窗口按 mac 惯例保留）。路由与 ui/layout/header.html
	// 的 menus 保持一致；/admin 不在列——壳层无法判断权限，平台路由守卫自带权限控制。
	navigate := func(label, route, accel string) *application.MenuItem {
		return application.NewMenuItem(label).
			SetAccelerator(accel).
			OnClick(func(*application.Context) { svc.Navigate(route) })
	}
	// 顶级路由快捷键：保留 Ctrl/Cmd+1/2/3 功能但不显示——wails3 菜单项
	// SetAccelerator 会拼接 \t 快捷键文本且与菜单显示耦合，改用全局 KeyBinding
	// 注册（processKeyBinding 第三分支，三平台可用；map 覆盖幂等）
	kb := application.Get().KeyBinding
	kb.Add("CmdOrCtrl+1", func(_ application.Window) { svc.Navigate("/") })
	kb.Add("CmdOrCtrl+2", func(_ application.Window) { svc.Navigate("/a") })
	kb.Add("CmdOrCtrl+3", func(_ application.Window) { svc.Navigate("/os") })
	// 顶层动作菜单：win/linux 普通项直接点击；darwin 必须用子菜单承载——
	// macOS 主菜单栏只渲染带 submenu 的顶层项（AppKit 行为），无 submenu 的
	// 普通项不显示（实测首页/Agents/OS/设置全消失）。
	topAction := func(label string, fn func()) {
		if runtime.GOOS == "darwin" {
			m := menu.AddSubmenu(label)
			addItems(m, application.NewMenuItem(label).OnClick(func(*application.Context) { fn() }))
			return
		}
		menu.Add(label).OnClick(func(*application.Context) { fn() })
	}
	topAction("首页", func() { svc.Navigate("/") })
	topAction("Agents", func() { svc.Navigate("/a") })
	topAction("OS", func() { svc.Navigate("/os") })
	// 设置：主窗口打开本地设置页（darwin 应用菜单内已有「设置…」Cmd+,；
	// win/linux 保留 Ctrl+, 功能但不显示——与首页/Agents/OS 同策略，全局 KeyBinding）
	if runtime.GOOS != "darwin" {
		kb.Add("CmdOrCtrl+,", func(_ application.Window) { svc.openSettings() })
	}
	topAction("设置", func() { svc.openSettings() })

	// 菜单：其余平台路由
	more := menu.AddSubmenu("菜单")
	addItems(
		more,
		navigate("控制台", "/settings/", "CmdOrCtrl+4"),
		navigate("设备", "/hosts", "CmdOrCtrl+5"),
		navigate("Skills", "/skills", "CmdOrCtrl+6"),
		navigate("Voices", "/voices", "CmdOrCtrl+7"),
	)

	// 编辑：darwin 必须补角色（见文件头注释），webview 内经 responder chain 原生响应；
	// win/linux 删除——WebView2/GTK 剪贴板原生支持，无需菜单项
	if runtime.GOOS == "darwin" {
		edit := menu.AddSubmenu("编辑")
		addItems(
			edit,
			menuItem(application.NewUndoMenuItem, "撤销", application.Undo),
			menuItem(application.NewRedoMenuItem, "重做", application.Redo),
			application.NewMenuItemSeparator(),
			menuItem(application.NewCutMenuItem, "剪切", application.Cut),
			menuItem(application.NewCopyMenuItem, "复制", application.Copy),
			menuItem(application.NewPasteMenuItem, "粘贴", application.Paste),
			menuItem(application.NewSelectAllMenuItem, "全选", application.SelectAll),
		)
	}

	// 视图：构造函数自带全平台 OnClick（Reload/Zoom/Fullscreen 无 darwin selector 映射）
	view := menu.AddSubmenu("视图")
	addItems(
		view,
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

	// 窗口：关闭触发现有 WindowClosing hook（= 隐藏窗口，常驻托盘语义不变）；
	// win/linux 删除——系统窗口按钮 + 托盘承担
	if runtime.GOOS == "darwin" {
		winm := menu.AddSubmenu("窗口")
		addItems(
			winm,
			menuItem(application.NewMinimiseMenuItem, "最小化", application.NoRole),
			menuItem(application.NewCloseMenuItem, "关闭窗口", application.NoRole),
			application.NewMenuItemSeparator(),
			application.NewMenuItem("打开平台").OnClick(func(*application.Context) {
				svc.FocusPlatform()
			}),
		)
	}

	return menu
}
