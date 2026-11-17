'use strict'

const readline = require('node:readline')

function safeError(error) {
  return String(error?.message || error || 'native session request failed')
    .replace(/s1\.[0-9a-f]{36}\.\d+\.\S+/g, '[redacted session token]')
    .replace(/eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+/g, '[redacted jwt]')
}

// Electron 端私有 stdio RPC。stdout 只接收协议帧，任何日志行都是协议错误。
function attachSessionRPC(child, { bootstrap, accessToken, onError = () => {} }) {
  const handlers = { session_bootstrap: bootstrap, access_token: accessToken }
  const lines = readline.createInterface({ input: child.stdout, crlfDelay: Infinity })
  let closed = false

  lines.on('line', async line => {
    if (closed || !line.trim()) return
    let request
    try {
      request = JSON.parse(line)
      if (!request || typeof request.id !== 'string' || !request.id || typeof request.method !== 'string') {
        throw new Error('invalid desktop session request')
      }
    } catch (error) {
      onError(new Error('Desktop 子进程输出了非法控制帧'))
      return
    }
    const handler = handlers[request.method]
    if (typeof handler !== 'function') {
      write({ id: request.id, error: 'unsupported desktop session method' })
      return
    }
    try {
      const result = await handler(request.params || {})
      write({ id: request.id, result: result === undefined ? {} : result })
    } catch (error) {
      write({ id: request.id, error: safeError(error) })
    }
  })
  lines.on('error', error => onError(error))

  function write(frame) {
    if (closed || child.stdin.destroyed || child.killed) return
    try { child.stdin.write(JSON.stringify(frame) + '\n') } catch (error) { onError(error) }
  }

  function close() {
    if (closed) return
    closed = true
    lines.close()
  }
  child.once('close', close)
  return { close }
}

module.exports = { attachSessionRPC, safeError }
