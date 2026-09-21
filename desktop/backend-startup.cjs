const fs = require('node:fs')
const { stripVTControlCharacters } = require('node:util')

// 端口文件只承载启动握手。失败时保留有限日志尾部，供桌面端显示具体原因。
function waitForBackend(child, portFile, { timeoutMs = 15000, pollMs = 100 } = {}) {
  return new Promise((resolve, reject) => {
    let tail = ''
    let timer
    let deadlineTimer
    let settled = false
    const capture = (data) => { tail = (tail + data.toString()).slice(-8192) }
    const finish = (error, info) => {
      if (settled) return
      settled = true
      clearTimeout(timer)
      clearTimeout(deadlineTimer)
      child.stdout?.off('data', capture)
      child.stderr?.off('data', capture)
      child.off('error', onError)
      child.off('exit', onExit)
      child.off('close', onClose)
      const diagnostic = stripVTControlCharacters(tail).trim()
      if (error) reject(new Error(error + (diagnostic ? '\n\n' + diagnostic : '')))
      else resolve(info)
    }
    const onError = (error) => finish(`无法启动本地服务：${error.message}`)
    // close 在 stdio 排空后触发；退出时停止读握手，等日志到齐再报告错误。
    const onExit = () => { clearTimeout(timer) }
    const onClose = (code, signal) => finish(`本地服务在启动完成前退出（${signal || code}）`)
    const poll = () => {
      try {
        const info = JSON.parse(fs.readFileSync(portFile, 'utf8'))
        if (!info || !Number.isInteger(info.port) || info.port < 1 || info.port > 65535 ||
            typeof info.code !== 'string' || !info.code.trim()) {
          finish('本地服务启动握手无效：缺少有效端口或校验码')
          return
        }
        finish(null, { port: info.port, code: info.code })
        return
      } catch (error) {
        // 文件尚未创建或正在写入时重试；权限等文件错误直接报告。
        if (error.code !== 'ENOENT' && !(error instanceof SyntaxError)) {
          finish(`无法读取本地服务启动信息：${error.message}`)
          return
        }
      }
      timer = setTimeout(poll, pollMs)
    }
    child.stdout?.on('data', capture)
    child.stderr?.on('data', capture)
    child.once('error', onError)
    child.once('exit', onExit)
    child.once('close', onClose)
    deadlineTimer = setTimeout(() => finish(`本地服务启动超时（${timeoutMs / 1000} 秒），未收到完整启动信息`), timeoutMs)
    if (child.exitCode != null || child.signalCode != null) onClose(child.exitCode, child.signalCode)
    else poll()
  })
}

module.exports = { waitForBackend }
