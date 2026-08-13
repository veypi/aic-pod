// settings-preload.js — 本地设置窗口专用（独立 partition，仅 127.0.0.1 本地页面）。
// 只暴露配置流程需要的两个能力：探测平台可达 + 打开平台（主进程切主窗口）。
const { contextBridge, ipcRenderer } = require('electron')

contextBridge.exposeInMainWorld('aicDesktop', {
  isDesktop: true,
  // 探测 {host}/root.html 是否可达（主进程 net 请求，无 CORS 限制）
  checkPlatform: (host) => ipcRenderer.invoke('platform:check', host),
  // 主窗口跳转平台 {url} + 关闭设置窗口
  openPlatform: (url) => ipcRenderer.invoke('platform:open', url),
})
