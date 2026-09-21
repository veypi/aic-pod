const { stripVTControlCharacters } = require('node:util')

// 后端子进程启动确认（2026-09-22 去 AIC_PORT_FILE 端口握手后）。
//
// 后端只做平台连接、不再监听本地端口，没有可读的启动凭证文件；判定标准改为：
// 子进程在 graceMs 窗口内既未退出也未报错 → 视为就绪；提前失败时抛出携带日志尾部
// 的错误（启动失败原因通常是凭证/配置问题，日志尾部最有诊断价值）。
function waitForStartup(child, { graceMs = 600 } = {}) {
  return new Promise((resolve, reject) => {
    let tail = ''
    let settled = false
    let timer = null

    const capture = (data) => { tail = (tail + data.toString()).slice(-8192) }
    const diagnostic = () => {
      const d = stripVTControlCharacters(tail).trim()
      return d ? '\n\n' + d : ''
    }
    const finish = (error) => {
      if (settled) return
      settled = true
      clearTimeout(timer)
      child.stdout?.off('data', capture)
      child.stderr?.off('data', capture)
      child.off('error', onError)
      child.off('exit', onExit)
      child.off('close', onClose)
      if (error) reject(new Error(error + diagnostic()))
      else resolve()
    }
    const onError = (error) => finish(`无法启动本地服务：${error.message}`)
    const onExit = (code, signal) => finish(`本地服务在启动完成前退出（${signal || code}）`)
    // close 在 stdio 排空后触发：先等日志到齐再报告错误（tail 才完整）。
    const onClose = (code, signal) => finish(`本地服务在启动完成前退出（${signal || code}）`)

    child.stdout?.on('data', capture)
    child.stderr?.on('data', capture)
    child.once('error', onError)
    child.once('exit', onExit)
    child.once('close', onClose)

    timer = setTimeout(() => finish(null), graceMs)
    // 已退出（或 spawn 直接失败）的子进程不必等窗口结束
    if (child.exitCode != null || child.signalCode != null) onClose(child.exitCode, child.signalCode)
  })
}

module.exports = { waitForStartup }
