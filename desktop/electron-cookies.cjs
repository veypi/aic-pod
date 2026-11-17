'use strict'

const { serverURL } = require('./auth-cookies.cjs')

function toElectronCookie(server, cookie, now = Date.now()) {
  const result = {
    url: serverURL(server, cookie.path || '/'),
    name: cookie.name,
    value: cookie.value,
    path: cookie.path || '/',
    secure: !!cookie.secure,
    httpOnly: true,
    sameSite: cookie.sameSite || 'strict',
  }
  if (cookie.expires > 0) result.expirationDate = cookie.expires / 1000
  else if (Number.isFinite(cookie.maxAge)) result.expirationDate = (now + cookie.maxAge * 1000) / 1000
  return result
}

function createElectronCookies({ server, session }) {
  const jar = session.defaultSession.cookies
  async function sync(credentials) {
    for (const cookie of [credentials.accessCookie, credentials.refreshCookie]) {
      if (!cookie) continue
      await jar.set(toElectronCookie(server, cookie))
    }
    await jar.flushStore()
  }
  async function clear(credentials) {
    for (const cookie of [credentials.accessCookie, credentials.refreshCookie]) {
      if (!cookie) continue
      const item = toElectronCookie(server, cookie)
      await jar.remove(item.url, item.name)
    }
    await jar.flushStore()
  }
  // 读回当前站点的会话 cookie（第三方授权在应用内完成后据此接管会话）。
  async function pull() {
    const items = await jar.get({ url: serverURL(server, '/') })
    return items.map(item => ({
      name: item.name,
      value: item.value,
      path: item.path || '/',
      secure: !!item.secure,
      httpOnly: !!item.httpOnly,
      sameSite: item.sameSite || 'strict',
    }))
  }
  return { sync, clear, pull }
}

module.exports = { createElectronCookies, toElectronCookie }
