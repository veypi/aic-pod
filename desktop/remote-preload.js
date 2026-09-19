// remote-preload.js — 注入 defaultSession 全部 frame（Electron 35+ session.registerPreloadScript）。
// 仅平台白名单 host（配置 host + 默认域名与旧域名）：完整能力（api 转发/窗口控制/
// 外链/桌宠）。浏览器命令通过 hosts/1 设备连接调用。
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
  })
}
