// settings-preload.js — 独立本地配置窗口专用（BrowserWindow，独立 partition，仅 app://aic 设置页）。
// 只暴露配置流程需要的能力：设置桥（api）+ 探测平台可达 + 打开平台。
// api 仅服务本地受信任设置页；主进程执行配置、日志与原生 Session Host 管理。
// 平台工作区不暴露这个通用入口，只使用 remote-preload 的固定管理方法。
const { contextBridge, ipcRenderer } = require('electron')

contextBridge.exposeInMainWorld('aicDesktop', {
  isDesktop: true,
  // 设置面（方法名与平台页 window.aicDesktop.api 一致）
  api: (method, args) => ipcRenderer.invoke('local:api', method, args === undefined ? null : args),
  // 探测 {host}/root.html 是否可达（主进程 net 请求，无 CORS 限制）
  checkPlatform: (host) => ipcRenderer.invoke('platform:check', host),
  // 主窗口跳转平台 {url}（设置视图保留，不关闭）
  openPlatform: (url) => ipcRenderer.invoke('platform:open', url),
})
