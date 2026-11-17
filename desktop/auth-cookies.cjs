'use strict'

// 原生认证只从 Set-Cookie 取得 Web token；响应 body 永不作为 token 来源。
// 本模块不依赖 Electron，便于对 Cookie 边界和 JWT 身份做确定性单测。
function normalizeServer(raw) {
  let value = String(raw || '').trim()
  if (!value) throw new Error('平台地址不能为空')
  if (!/^[a-z][a-z0-9+.-]*:\/\//i.test(value)) value = 'https://' + value
  const url = new URL(value)
  if (!/^https?:$/.test(url.protocol) || !url.host || url.username || url.password) {
    throw new Error('平台地址只支持 http/https')
  }
  url.search = ''
  url.hash = ''
  return url.href.replace(/\/+$/, '')
}

function serverURL(server, path) {
  return normalizeServer(server) + '/' + String(path || '').replace(/^\/+/, '')
}

function serverOrigin(server) {
  return new URL(normalizeServer(server)).origin
}

function splitSetCookie(value) {
  // Expires 的逗号后不是 token=，因此不会误切日期；新 Cookie 的逗号后一定是 name=。
  return String(value || '').split(/,(?=\s*[!#$%&'*+\-.^_`|~0-9A-Za-z]+=)/).map(item => item.trim()).filter(Boolean)
}

function responseSetCookies(headers) {
  if (!headers) return []
  if (typeof headers.getSetCookie === 'function') return headers.getSetCookie()
  const value = typeof headers.get === 'function' ? headers.get('set-cookie') : ''
  return splitSetCookie(value)
}

function parseSetCookie(line) {
  const parts = String(line || '').split(';')
  const first = parts.shift() || ''
  const index = first.indexOf('=')
  if (index <= 0) return null
  const cookie = {
    name: first.slice(0, index).trim(),
    value: first.slice(index + 1).trim(),
    path: '/',
    secure: false,
    httpOnly: false,
    sameSite: 'strict',
    expires: 0,
  }
  for (const part of parts) {
    const i = part.indexOf('=')
    const key = (i < 0 ? part : part.slice(0, i)).trim().toLowerCase()
    const value = i < 0 ? '' : part.slice(i + 1).trim()
    if (key === 'path' && value.startsWith('/')) cookie.path = value
    else if (key === 'secure') cookie.secure = true
    else if (key === 'httponly') cookie.httpOnly = true
    else if (key === 'samesite') cookie.sameSite = value.toLowerCase() === 'lax' ? 'lax' : value.toLowerCase() === 'none' ? 'no_restriction' : 'strict'
    else if (key === 'expires') {
      const time = Date.parse(value)
      if (Number.isFinite(time)) cookie.expires = time
    } else if (key === 'max-age') {
      const seconds = Number(value)
      if (Number.isFinite(seconds)) cookie.maxAge = seconds
    }
  }
  return cookie
}

function decodeJWT(token) {
  const parts = String(token || '').split('.')
  if (parts.length !== 3) throw new Error('非法访问凭证')
  const payload = JSON.parse(Buffer.from(parts[1], 'base64url').toString('utf8'))
  if (!payload || typeof payload !== 'object') throw new Error('非法访问凭证')
  return payload
}

function tokenUID(token) {
  const payload = decodeJWT(token)
  const uid = String(payload.uid || payload.sub || '')
  if (!/^[0-9a-f]{32}$/.test(uid)) throw new Error('访问凭证缺少有效用户身份')
  return uid
}

function credentialsFromResponse(response, body, now = Date.now()) {
  const cookies = responseSetCookies(response.headers).map(parseSetCookie).filter(Boolean)
  let accessCookie = cookies.find(cookie => /access$/i.test(cookie.name))
  let refreshCookie = cookies.find(cookie => /refresh$/i.test(cookie.name))
  if (!accessCookie) {
    accessCookie = cookies.find(cookie => {
      try { return decodeJWT(cookie.value).type === 'access' } catch (_) { return false }
    })
  }
  if (!refreshCookie) {
    refreshCookie = cookies.find(cookie => {
      try { return decodeJWT(cookie.value).type === 'refresh' } catch (_) { return false }
    })
  }
  if (!accessCookie || !refreshCookie || !accessCookie.value || !refreshCookie.value || !accessCookie.httpOnly || !refreshCookie.httpOnly) {
    throw new Error('登录响应缺少 HttpOnly 会话凭证')
  }
  const payload = decodeJWT(accessCookie.value)
  if (payload.type !== 'access' || payload.mod !== 'inherit') {
    throw new Error('登录响应不是动态 Web access token')
  }
  const uid = tokenUID(accessCookie.value)
  const refreshPayload = decodeJWT(refreshCookie.value)
  if (refreshPayload.type !== 'refresh' || tokenUID(refreshCookie.value) !== uid) {
    throw new Error('登录响应的 refresh token 与用户身份不一致')
  }
  if (body?.user?.id && body.user.id !== uid) throw new Error('登录用户与访问凭证不一致')
  const expiresAt = Number.isFinite(payload.exp)
    ? payload.exp * 1000
    : now + Math.max(1, Number(body?.expires_in) || 900) * 1000
  return {
    accessToken: accessCookie.value,
    refreshToken: refreshCookie.value,
    accessCookie,
    refreshCookie,
    cookies,
    user: body?.user || { id: uid },
    expiresAt,
  }
}

// credentialsFromCookies 从会话 cookie（第三方授权在应用内完成后）重建凭据。
// 与 credentialsFromResponse 同口径：只认 HttpOnly 的 access + refresh，且两个 token 同属一个用户。
function credentialsFromCookies(cookies, now = Date.now()) {
  const list = Array.isArray(cookies) ? cookies : []
  const pick = (suffix, type) => {
    const byName = list.find(item => new RegExp(suffix + '$', 'i').test(String(item.name || '')))
    if (byName) return byName
    return list.find(item => {
      try { return decodeJWT(String(item.value || '')).type === type } catch (_) { return false }
    })
  }
  const access = pick('access', 'access')
  const refresh = pick('refresh', 'refresh')
  if (!access || !refresh || !access.value || !refresh.value) return null
  const payload = decodeJWT(access.value)
  if (payload.type !== 'access' || payload.mod !== 'inherit') return null
  const uid = tokenUID(access.value)
  const refreshPayload = decodeJWT(refresh.value)
  if (refreshPayload.type !== 'refresh' || tokenUID(refresh.value) !== uid) return null
  const expiresAt = Number.isFinite(payload.exp) ? payload.exp * 1000 : now + 900 * 1000
  const cookieOf = item => ({
    name: String(item.name || ''),
    value: String(item.value || ''),
    path: item.path || '/',
    secure: !!item.secure,
    httpOnly: true,
    sameSite: 'strict',
    expires: 0,
    maxAge: NaN,
  })
  return {
    accessToken: access.value,
    refreshToken: refresh.value,
    accessCookie: cookieOf(access),
    refreshCookie: cookieOf(refresh),
    cookies: [],
    user: { id: uid },
    expiresAt,
  }
}

function publicAuthState(credentials) {  if (!credentials) return { signedIn: false, user: null, expiresAt: 0 }
  return {
    signedIn: true,
    user: credentials.user || { id: tokenUID(credentials.accessToken) },
    expiresAt: credentials.expiresAt || 0,
  }
}

function validateSessionProfile(profile, uid) {
  const value = profile && typeof profile === 'object' ? profile : {}
  if (!/^[0-9a-f]{32}$/.test(String(uid || '')) || value.uid !== uid) {
    throw new Error('Host 档案用户与当前账户不一致')
  }
  if (typeof value.host_id !== 'string' || !value.host_id.startsWith(uid) || !/^[0-9a-f]{32}[0-9]{4}$/.test(value.host_id) || value.host_id.endsWith('0000')) {
    throw new Error('ensure 返回了非法 Host ID')
  }
  if (!Number.isSafeInteger(value.credential_ver) || value.credential_ver <= 0) {
    throw new Error('ensure 返回了非法凭证代次')
  }
  if (typeof value.protocol_secret !== 'string' || !value.protocol_secret.trim()) {
    throw new Error('ensure 未返回 Host protocol secret')
  }
  return {
    host_id: value.host_id,
    uid,
    credential_ver: value.credential_ver,
    protocol_secret: value.protocol_secret,
  }
}

module.exports = {
  normalizeServer,
  serverURL,
  serverOrigin,
  splitSetCookie,
  responseSetCookies,
  parseSetCookie,
  decodeJWT,
  tokenUID,
  credentialsFromResponse,
  credentialsFromCookies,
  publicAuthState,
  validateSessionProfile,
}
