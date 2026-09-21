// settings-preload.js — 独立本地配置窗口专用（BrowserWindow，独立 partition，仅 app://aic 设置页）。
// 只暴露配置流程需要的能力：设置桥（api）+ 探测平台可达 + 打开平台。
// api(method, args)：与平台页 remote-preload 同一路由（IPC 'local:api'），主进程按方法名
// spawn `aic-backend config|bind` 子命令 / 读日志 / 重启后端子进程——本地无 HTTP、无校验码。
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
