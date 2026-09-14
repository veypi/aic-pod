// remote-preload.js — 注入 defaultSession 全部 frame（Electron 35+ session.registerPreloadScript）。
// 仅平台白名单 host（配置 host + 默认域名与旧域名）：完整能力（api 转发/窗口控制/
// 外链/桌宠 + nativeWin 原生内容桥——OS 原生窗口内容协议，设计见
// aic/docs/os_native_windows.md §3）。
// 本地设置页不在本 preload 范围：独立配置窗口走 settings-preload（独立 partition）。
const { contextBridge, ipcRenderer } = require('electron')

// 白名单（主进程下发；allowed:hosts 返回平台 host 数组）
let allowedHosts = []
try {
  const m = ipcRenderer.sendSync('allowed:hosts')
  if (Array.isArray(m)) allowedHosts = m
} catch (e) { /* 忽略 */ }

if (allowedHosts.includes(location.host)) {
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
      // v2：洞内输入转发（页面侧命中判定 → 壳侧坐标翻译 + sendInputEvent + 焦点转移；
      // 设计见 aic/docs/os_native_windows.md §6）
      mouse: (m) => ipcRenderer.send('native:mouse', m || {}),
      wheel: (m) => ipcRenderer.send('native:wheel', m || {}),
      // leader 键抓取（docs §6）：页面同步 leader 集合（页面为配置唯一源，改键跟随；
      // 未同步 = 壳不抓取）；native:keys = leader 会话进入事件（原生内容聚焦时壳侧
      // 吞下的 leader 按下）→ 页面合成事件走既有 keymap/编排链路，此后物理键由平台页
      // 原生接收；focus = 平台页保留键盘焦点（launcher 等需要输入的动作，leader 释放
      // 后不自动交还内容视图）
      setLeader: (mods) => ipcRenderer.invoke('native:leader', Array.isArray(mods) ? mods.map((m) => String(m)) : []),
      onKeys: (fn) => {
        const h = (e, ev) => fn(ev)
        ipcRenderer.on('native:keys', h)
        return () => ipcRenderer.removeListener('native:keys', h)
      },
      focus: () => ipcRenderer.invoke('native:focus'),
      // 标签集变化（全量推送）；返回取消函数
      onChanged: (fn) => {
        const h = (e, st) => fn(st)
        ipcRenderer.on('native:changed', h)
        return () => ipcRenderer.removeListener('native:changed', h)
      },
    },
  })
}
