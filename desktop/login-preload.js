// 原生登录页专用桥。只在 app://aic/login.html 暴露，平台工作区无法调用。
const { contextBridge, ipcRenderer } = require('electron')

if (location.protocol === 'app:' && location.hostname === 'aic' && location.pathname === '/login.html') {
  contextBridge.exposeInMainWorld('aicNative', {
    call: (method, params) => ipcRenderer.invoke('native:auth', method, params === undefined ? null : params),
    onSync: handler => {
      if (typeof handler !== 'function') throw new TypeError('handler must be a function')
      const listener = () => handler()
      ipcRenderer.on('native:auth:sync', listener)
      return () => ipcRenderer.removeListener('native:auth:sync', listener)
    },
  })
}
