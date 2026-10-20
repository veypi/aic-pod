'use strict'

// Go 后端子进程生命周期：spawn / 优雅停止 / 串行重启 / 退出回收。
// Electron 只提供 spawn、环境与致命错误回调；窗口/对话框不在本模块。
function createBackend({ spawn, binary, env, waitForStartup, onFatal }) {
  let child = null
  let ready = false // 就绪后异常退出才算启动失败（主动 stop/restart 不算）
  let restarting = null

  function alive() {
    return !!child && !child.killed && child.exitCode == null && child.signalCode == null
  }

  async function start() {
    child = spawn(binary, [], { env, stdio: ['ignore', 'pipe', 'pipe'] })
    child.stdout.on('data', (d) => console.log('[backend]', d.toString().trim()))
    child.stderr.on('data', (d) => console.log('[backend]', d.toString().trim()))
    child.on('exit', (code, signal) => {
      if (ready) onFatal(new Error(`后端进程异常退出（${signal || code}）`))
      ready = false
    })
    child.on('error', (error) => {
      if (ready) onFatal(error)
      ready = false
    })
    await waitForStartup(child)
    ready = true
    return child
  }

  async function stop() {
    const current = child
    ready = false // 主动停止：退出不再算启动失败
    child = null
    if (!current || current.exitCode != null || current.signalCode != null) return
    await new Promise((resolve) => {
      current.once('close', () => resolve())
      try { current.kill('SIGTERM') } catch (e) { /* 已退出 */ }
      const timer = setTimeout(() => {
        try { current.kill('SIGKILL') } catch (e) { /* 忽略 */ }
        resolve()
      }, 5000)
      timer.unref?.()
    })
  }

  function restart() {
    if (!restarting) {
      restarting = (async () => { await stop(); await start() })().finally(() => { restarting = null })
    }
    return restarting
  }

  function terminate() {
    if (child && !child.killed) child.kill('SIGTERM')
  }

  return {
    start,
    stop,
    restart,
    alive,
    terminate,
    get child() { return child },
  }
}

module.exports = { createBackend }
