// 原生登录页专用桥。只在 app://aic/login.html 暴露，平台工作区无法调用。
const { contextBridge, ipcRenderer } = require('electron')

if (location.protocol === 'app:' && location.hostname === 'aic' && location.pathname === '/login.html') {
  const api = {
    call: (method, params) => ipcRenderer.invoke('native:auth', method, params === undefined ? null : params),
    onSync: handler => {
      if (typeof handler !== 'function') throw new TypeError('handler must be a function')
      const listener = () => handler()
      ipcRenderer.on('native:auth:sync', listener)
      return () => ipcRenderer.removeListener('native:auth:sync', listener)
    },
  }
  try {
    contextBridge.exposeInMainWorld('aicNative', api)
  } catch (_) {
    // session preload 与窗口 preload 同时注入时，只暴露一次。
  }
}
