'use strict'

// 设置窗自定义协议（app://aic/<path> → desktop/settings-ui/<path>）：
// 无本地 HTTP 服务、无构建。registerScheme 必须在 app ready 前调用；
// registerHandler 在 ready 后把默认 session 与 settings partition 都挂上。
const fs = require('fs')
const path = require('path')

const SETTINGS_SCHEME = 'app'
const SETTINGS_ORIGIN = 'app://aic'
const SETTINGS_PAGE = '/settings.html'
const SETTINGS_MIME = {
  '.html': 'text/html',
  '.js': 'text/javascript',
  '.mjs': 'text/javascript',
  '.css': 'text/css',
  '.json': 'application/json',
  '.svg': 'image/svg+xml',
  '.png': 'image/png',
  '.jpg': 'image/jpeg',
  '.webp': 'image/webp',
  '.woff': 'font/woff',
  '.woff2': 'font/woff2',
  '.ttf': 'font/ttf',
  '.ico': 'image/x-icon',
}

function registerScheme(protocol) {
  protocol.registerSchemesAsPrivileged([
    { scheme: SETTINGS_SCHEME, privileges: { standard: true, secure: true, supportFetchAPI: true, corsEnabled: true, stream: true } },
  ])
}

function registerHandler({ protocol, session, uiDir }) {
  const handler = async (request) => {
    let rel = SETTINGS_PAGE
    try { rel = decodeURIComponent(new URL(request.url).pathname || SETTINGS_PAGE) } catch (e) { /* 忽略 */ }
    if (rel === '/') rel = SETTINGS_PAGE
    const notFound = () => new Response('not found', { status: 404, headers: { 'Content-Type': 'text/plain' } })
    const file = path.resolve(uiDir, '.' + path.posix.normalize(rel))
    if (file !== uiDir && !file.startsWith(uiDir + path.sep)) return notFound() // 目录穿越护栏
    try {
      const body = await fs.promises.readFile(file)
      const mime = SETTINGS_MIME[path.extname(file).toLowerCase()] || 'application/octet-stream'
      return new Response(body, { headers: { 'Content-Type': mime } })
    } catch (e) { return notFound() }
  }
  // 默认 session + 设置窗独立 partition：自定义协议逐 session 注册，缺一处即 ERR_UNKNOWN_URL_SCHEME
  protocol.handle(SETTINGS_SCHEME, handler)
  try {
    session.fromPartition('settings').protocol.handle(SETTINGS_SCHEME, handler)
  } catch (e) {
    console.error('[settings] protocol register failed:', e.message)
  }
}

module.exports = {
  SETTINGS_SCHEME,
  SETTINGS_ORIGIN,
  SETTINGS_PAGE,
  registerScheme,
  registerHandler,
}
