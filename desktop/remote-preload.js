// remote-preload.js — 注入远端平台页（session.setPreloads，所有 frame 生效）。
// 白名单（主进程下发：配置 host + https://ivec.ai）过滤后才暴露能力；
// 端口/code 由主进程持有，平台页完全无感。
const { contextBridge, ipcRenderer } = require('electron')

let allowed = []
try {
  allowed = ipcRenderer.sendSync('allowed:hosts') || []
} catch (e) { /* 忽略 */ }
if (!Array.isArray(allowed) || !allowed.includes(location.host)) return

contextBridge.exposeInMainWorld('aicDesktop', {
  isDesktop: true,
  // 本地 API 转发（主进程带 code 请求本地服务，方法名区分 GET/POST）
  api: (method, args) => ipcRenderer.invoke('local:api', method, args === undefined ? null : args),
  // 窗口控制（Electron 主进程操作主窗口）
  minimise: () => ipcRenderer.invoke('window:minimise'),
  maximise: () => ipcRenderer.invoke('window:maximise'),
  close: () => ipcRenderer.invoke('window:close'),
  fullscreen: () => ipcRenderer.invoke('window:fullscreen'),
  pet: (x, y) => ipcRenderer.invoke('window:pet', x, y),
  restore: () => ipcRenderer.invoke('window:restore'),
  // 外链 → 系统默认浏览器（仅 http/https，主进程校验）
  openExternal: (url) => ipcRenderer.invoke('open:external', url),
  // 桌宠拖动（pet 页 mousedown/mousemove 屏幕坐标）
  petDragStart: (x, y) => ipcRenderer.send('pet:drag-start', { x, y }),
  petDragMove: (x, y) => ipcRenderer.send('pet:drag-move', { x, y }),
})
