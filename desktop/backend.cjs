'use strict'

// Go 后端子进程生命周期：spawn / 优雅停止 / 串行重启 / 退出回收。
// Electron 只提供 spawn、环境与致命错误回调；窗口/对话框不在本模块。
function createBackend({ spawn, binary, env, waitForStartup, onFatal }) {
  let child = null
  let ready = false // 就绪后异常退出才算启动失败（主动 stop/restart 不算）
  let restarting = null
  let queue = Promise.resolve()

  // start/stop/restart 串行化：任一时刻最多一个生命周期操作在途——并发
  // start+stop 不会出现「stop 刚置空 child、start 又起新进程」的新旧并存。
  function enqueue(job) {
    const run = queue.then(job, job)
    queue = run.then(() => {}, () => {})
    return run
  }

  function alive() {
    return !!child && !child.killed && child.exitCode == null && child.signalCode == null
  }

  async function startNow() {
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

  async function stopNow() {
    const current = child
    ready = false // 主动停止：退出不再算启动失败
    child = null
    if (!current || current.exitCode != null || current.signalCode != null) return
    await new Promise((resolve) => {
      // 'close' 可能已先于监听注册触发（exit 已记录即已关闭，上面已拦）——
      // 复查一次避免白等满超时。
      if (current.exitCode != null || current.signalCode != null) return resolve()
      current.once('close', () => resolve())
      try { current.kill('SIGTERM') } catch (e) { /* 已退出 */ }
      const timer = setTimeout(() => {
        try { current.kill('SIGKILL') } catch (e) { /* 忽略 */ }
        resolve()
      }, 5000)
      timer.unref?.()
    })
  }

  const start = () => enqueue(startNow)
  const stop = () => enqueue(stopNow)

  function restart() {
    if (!restarting) {
      restarting = enqueue(async () => { await stopNow(); await startNow() }).finally(() => { restarting = null })
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
