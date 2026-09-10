// settings-preload.js — 本地设置页 hostView 专用（独立 partition，仅 127.0.0.1 本地页面）。
// 只暴露配置流程需要的能力：探测平台可达 + 打开平台（主进程切主窗口）+ 关闭设置视图。
// （主视图整窗加载形态由 remote-preload 本地分支提供子集，无 closeSettings。）
const { contextBridge, ipcRenderer } = require('electron')

contextBridge.exposeInMainWorld('aicDesktop', {
  isDesktop: true,
  // 探测 {host}/root.html 是否可达（主进程 net 请求，无 CORS 限制）
  checkPlatform: (host) => ipcRenderer.invoke('platform:check', host),
  // 主窗口跳转平台 {url}（设置视图保留，不关闭）
  openPlatform: (url) => ipcRenderer.invoke('platform:open', url),
  // 关闭设置视图（hostView 内嵌模式；浏览器直开/主视图整窗无此能力）
  closeSettings: () => ipcRenderer.invoke('settings:close'),
})
