// remote-preload.js — 受信任平台 origin 的 App 能力与 Desktop 自有管理桥。
// aicHost：三端统一协议；aicDesktop：窗口、桌宠和本地 Pod 管理，不承载 Web 认证。
const { contextBridge, ipcRenderer, webUtils } = require('electron')

let allowedHosts = []
try {
  const hosts = ipcRenderer.sendSync('allowed:hosts')
  if (Array.isArray(hosts)) allowedHosts = hosts
} catch (_) { /* 忽略 */ }

if (allowedHosts.includes(location.host)) {
  const listeners = new Map()
  const onBridgeEvent = (_event, message) => {
    const handlers = listeners.get(message?.event)
    if (!handlers) return
    for (const handler of [...handlers]) handler(message.payload)
  }
  ipcRenderer.on('aic-host:event', onBridgeEvent)

  contextBridge.exposeInMainWorld('aicHost', {
    version: 1,
    platform: 'desktop',
    capabilities: {
      auth: true,
      share: false,
      clipboard: false,
      files: false,
      notify: false,
      external: true,
      deeplink: false,
      theme: false,
      locale: false,
      accountDelete: false,
    },
    call: (method, params) => ipcRenderer.invoke('aic-host:call', method, params === undefined ? null : params),
    on: (event, handler) => {
      if (typeof handler !== 'function') throw new TypeError('handler must be a function')
      let handlers = listeners.get(event)
      if (!handlers) {
        handlers = new Set()
        listeners.set(event, handlers)
      }
      handlers.add(handler)
      return () => {
        handlers.delete(handler)
        if (!handlers.size) listeners.delete(event)
      }
    },
    off: (event, handler) => {
      const handlers = listeners.get(event)
      handlers?.delete(handler)
      if (handlers && !handlers.size) listeners.delete(event)
    },
  })

  contextBridge.exposeInMainWorld('aicDesktop', {
    isDesktop: true,
    sessionAuth: true,
    // Desktop 自有 Pod 管理面：固定方法，不向普通网页暴露任意 IPC method。
    getConfig: () => ipcRenderer.invoke('local:api', 'get_config', null),
    setConfig: config => ipcRenderer.invoke('local:api', 'set_config', config),
    getStatus: () => ipcRenderer.invoke('local:api', 'get_status', null),
    getLog: () => ipcRenderer.invoke('local:api', 'get_log', null),
    start: () => ipcRenderer.invoke('local:api', 'start', null),
    stop: () => ipcRenderer.invoke('local:api', 'stop', null),
    pathForFile: file => webUtils.getPathForFile(file),
    minimise: () => ipcRenderer.invoke('window:minimise'),
    maximise: () => ipcRenderer.invoke('window:maximise'),
    close: () => ipcRenderer.invoke('window:close'),
    fullscreen: () => ipcRenderer.invoke('window:fullscreen'),
    pet: page => ipcRenderer.invoke('window:pet', page),
    restore: () => ipcRenderer.invoke('window:restore'),
    screenshot: () => ipcRenderer.invoke('platform:screenshot'),
    openExternal: url => ipcRenderer.invoke('open:external', url),
    petDragStart: (x, y) => ipcRenderer.send('pet:drag-start', { x, y }),
    petDragMove: (x, y) => ipcRenderer.send('pet:drag-move', { x, y }),
    petMenu: opts => ipcRenderer.send('pet:menu', opts || {}),
    onPetToggleDialog: fn => {
      const handler = () => fn()
      ipcRenderer.on('pet:toggle-dialog', handler)
      return () => ipcRenderer.removeListener('pet:toggle-dialog', handler)
    },
    onPetCmd: fn => {
      const handler = (_event, command) => fn(command)
      ipcRenderer.on('pet:cmd', handler)
      return () => ipcRenderer.removeListener('pet:cmd', handler)
    },
  })
}
