// remote-preload.js — 注入 defaultSession 全部 frame（Electron 35+ session.registerPreloadScript）。
// 按 frame 来源分两支（端口/code 由主进程持有，页面完全无感）：
//   - 平台白名单 host（配置 host + https://ivec.ai）：完整能力（api 转发/窗口控制/
//     外链/桌宠 + nativeWin 原生内容桥——OS 原生窗口内容协议，设计见
//     aic/docs/os_native_windows.md §3）。
//   - 本地设置页（127.0.0.1:<本地服务端口>，精确 host:port 匹配——平台开发态
//     常用 localhost:4000 同机不同端口，按 hostname 判会误吞平台页）：仅设置
//     子集（checkPlatform/openPlatform），不给 nativeWin/closeSettings（hostView
//     内嵌形态由 settings-preload 提供，走独立 partition 的 view 级 preload）。
const { contextBridge, ipcRenderer } = require('electron')

// 白名单 + 本地源（主进程下发；allowed:hosts 返回 {hosts, local}，local = 本地服务 host:port）
let allowedMeta = { hosts: [], local: '' }
try {
  const m = ipcRenderer.sendSync('allowed:hosts')
  if (m && Array.isArray(m.hosts)) allowedMeta = m
} catch (e) { /* 忽略 */ }

const isLocal = !!allowedMeta.local && location.host === allowedMeta.local

if (isLocal) {
  contextBridge.exposeInMainWorld('aicDesktop', {
    isDesktop: true,
    // 探测 {host}/root.html 是否可达（主进程 net 请求，无 CORS 限制）
    checkPlatform: (host) => ipcRenderer.invoke('platform:check', host),
    // 主窗口跳转平台 {url}
    openPlatform: (url) => ipcRenderer.invoke('platform:open', url),
  })
} else if (allowedMeta.hosts.includes(location.host)) {
  contextBridge.exposeInMainWorld('aicDesktop', {
    isDesktop: true,
    // 本地 API 转发（主进程带 code 请求本地服务，方法名区分 GET/POST）
    api: (method, args) => ipcRenderer.invoke('local:api', method, args === undefined ? null : args),
    // 窗口控制（Electron 主进程操作主窗口）
    minimise: () => ipcRenderer.invoke('window:minimise'),
    maximise: () => ipcRenderer.invoke('window:maximise'),
    close: () => ipcRenderer.invoke('window:close'),
    fullscreen: () => ipcRenderer.invoke('window:fullscreen'),
    pet: (page) => ipcRenderer.invoke('window:pet', page),
    restore: () => ipcRenderer.invoke('window:restore'),
    // 外链 → 系统默认浏览器（仅 http/https，主进程校验）
    openExternal: (url) => ipcRenderer.invoke('open:external', url),
    // 桌宠拖动（pet 页 mousedown/mousemove 屏幕坐标）与右键菜单
    petDragStart: (x, y) => ipcRenderer.send('pet:drag-start', { x, y }),
    petDragMove: (x, y) => ipcRenderer.send('pet:drag-move', { x, y }),
    // 右键菜单：携带页面状态（hasAgent/dialogVisible），主进程据此构建菜单项
    petMenu: (opts) => ipcRenderer.send('pet:menu', opts || {}),
    // 主进程菜单「打开/隐藏对话框」回投；返回取消函数
    onPetToggleDialog: (fn) => {
      const h = () => fn()
      ipcRenderer.on('pet:toggle-dialog', h)
      return () => ipcRenderer.removeListener('pet:toggle-dialog', h)
    },
    // CLI 指令回投（aic wake → 主进程 pet:cmd → pet 页）；返回取消函数
    onPetCmd: (fn) => {
      const h = (e, cmd) => fn(cmd)
      ipcRenderer.on('pet:cmd', h)
      return () => ipcRenderer.removeListener('pet:cmd', h)
    },
    // ---- nativeWin：OS 原生窗口内容桥（docs §3） ----
    // 渲染器是 rect/可见性的唯一驱动源；壳只执行贴位与 z 序。getState 为权威源。
    nativeWin: {
      getState: () => ipcRenderer.invoke('native:state'),
      createTab: (url) => ipcRenderer.invoke('native:tab-create', String(url || '')),
      closeTab: (id) => ipcRenderer.invoke('native:tab-close', id),
      activateTab: (id) => ipcRenderer.invoke('native:tab-activate', id),
      navigateTab: (id, url) => ipcRenderer.invoke('native:tab-navigate', id, String(url || '')),
      // {rect:{x,y,w,h}|null, visible:boolean}：rect=null/visible=false → 隐藏
      layout: (st) => ipcRenderer.invoke('native:layout', st),
      // {kind:'settings', rect, visible}：设置 hostView 贴位（懒创建/摘除保活）
      hostLayout: (st) => ipcRenderer.invoke('native:host-layout', st),
      // v2：洞内 wheel 转发（页面 listener 调用；主进程命中校验 + 符号换算后 sendInputEvent，
      // 见 aic/docs/os_native_windows.md §6——Electron before-mouse-event 不覆盖 wheel）
      wheel: (x, y, dx, dy, mode) => ipcRenderer.send('native:wheel', { x, y, dx, dy, mode }),
      // 标签集变化（全量推送）；返回取消函数
      onChanged: (fn) => {
        const h = (e, st) => fn(st)
        ipcRenderer.on('native:changed', h)
        return () => ipcRenderer.removeListener('native:changed', h)
      },
      // 托盘「本地配置」→ 平台页开设置窗口；返回取消函数
      onOpenHost: (fn) => {
        const h = (e, msg) => fn(msg)
        ipcRenderer.on('native:open-host', h)
        return () => ipcRenderer.removeListener('native:open-host', h)
      },
      // 设置页「关闭」→ 平台页关设置窗口；返回取消函数
      onHostClosed: (fn) => {
        const h = (e, msg) => fn(msg)
        ipcRenderer.on('native:host-closed', h)
        return () => ipcRenderer.removeListener('native:host-closed', h)
      },
    },
  })
}
