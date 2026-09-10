// divider-preload.js — A/B 分隔条专用（仅向主进程上报拖拽三个事件）
const { contextBridge, ipcRenderer } = require('electron')

contextBridge.exposeInMainWorld('divider', {
  down: () => ipcRenderer.send('b:divider-down'),
  move: (x) => ipcRenderer.send('b:divider-move', x),
  up: () => ipcRenderer.send('b:divider-up'),
})
