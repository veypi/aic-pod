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
    // rect 只决定画面展示和输入映射；getState 给出标签固定视口。
    nativeWin: {
      getState: () => ipcRenderer.invoke('native:state'),
      createTab: (url) => ipcRenderer.invoke('native:tab-create', String(url || '')),
      closeTab: (id) => ipcRenderer.invoke('native:tab-close', id),
      activateTab: (id) => ipcRenderer.invoke('native:tab-activate', id),
      navigateTab: (id, url) => ipcRenderer.invoke('native:tab-navigate', id, String(url || '')),
      // {rect:{x,y,w,h}|null, visible:boolean}：rect=null/visible=false → 隐藏
      layout: (st) => ipcRenderer.invoke('native:layout', st),
      // Fixed-viewport presentation: mouse coordinates are platform CSS pixels;
      // keyboard/IME focus stays in the viewer, browser input is delivered via CDP.
      mouse: (m) => ipcRenderer.send('native:mouse', m || {}),
      wheel: (m) => ipcRenderer.send('native:wheel', m || {}),
      resetInput: () => ipcRenderer.send('native:reset'),
      inputFocus: (focused) => ipcRenderer.send('native:input-focus', !!focused),
      key: (m) => ipcRenderer.send('native:key', m || {}),
      text: (text) => ipcRenderer.send('native:text', text),
      edit: (command) => ipcRenderer.send('native:edit', command),
      ackFrame: (seq) => ipcRenderer.send('native:frame-ack', seq),
      onFrame: (fn) => {
        const h = (_e, frame) => fn(frame)
        ipcRenderer.on('native:frame', h)
        return () => ipcRenderer.removeListener('native:frame', h)
      },
      // Shortcut configuration and compatibility hooks for older platform pages.
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
