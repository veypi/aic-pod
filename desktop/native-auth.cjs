'use strict'

const {
  normalizeServer,
  serverURL,
  serverOrigin,
  credentialsFromResponse,
  publicAuthState,
  tokenUID,
  validateSessionProfile,
} = require('./auth-cookies.cjs')

class NativeAuthError extends Error {
  constructor(message, { status = 0, code = 'auth_error' } = {}) {
    super(message)
    this.name = 'NativeAuthError'
    this.status = status
    this.code = code
  }
}

function createNativeAuth({
  server,
  fetch: fetchImpl = globalThis.fetch,
  store,
  syncCookies = async () => {},
  clearCookies = async () => {},
  device,
  onChanged = () => {},
  now = () => Date.now(),
}) {
  const base = normalizeServer(server)
  let credentials = null
  let refreshing = null

  function state() {
    return publicAuthState(credentials)
  }

  function emit() {
    const value = state()
    onChanged(value)
    return value
  }

  async function restore() {
    credentials = store.current()
    if (credentials) {
      tokenUID(credentials.accessToken)
      await syncCookies(credentials)
    }
    return state()
  }

  async function request(path, { method = 'GET', body, bearer, cookie, timeout = 20000 } = {}) {
    const headers = { Accept: 'application/json', Origin: serverOrigin(base) }
    if (body !== undefined) headers['Content-Type'] = 'application/json'
    if (bearer) headers.Authorization = 'Bearer ' + bearer
    if (cookie) headers.Cookie = cookie
    let response
    try {
      response = await fetchImpl(serverURL(base, path), {
        method,
        headers,
        body: body === undefined ? undefined : JSON.stringify(body),
        redirect: 'error',
        cache: 'no-store',
        signal: AbortSignal.timeout(timeout),
      })
    } catch (error) {
      throw new NativeAuthError('平台请求失败: ' + error.message, { code: 'network_error' })
    }
    const text = await response.text()
    let payload = null
    if (text) {
      try { payload = JSON.parse(text) } catch (_) { payload = null }
    }
    if (!response.ok) {
      const message = payload?.message || payload?.error?.message || (typeof payload?.error === 'string' ? payload.error : '') || `服务返回 ${response.status}`
      throw new NativeAuthError(message, { status: response.status, code: response.status === 401 ? 'authentication_required' : 'http_error' })
    }
    return { response, payload }
  }

  async function authenticate(path, body) {
    const { response, payload } = await request(path, { method: 'POST', body })
    const next = credentialsFromResponse(response, payload, now())
    const uid = tokenUID(next.accessToken)
    if (next.user.id !== uid) throw new NativeAuthError('登录用户身份不一致', { code: 'account_mismatch' })
    await store.saveLogin(next)
    credentials = next
    await syncCookies(credentials)
    return emit()
  }

  async function refresh() {
    if (!credentials) throw new NativeAuthError('未登录', { status: 401, code: 'authentication_required' })
    if (refreshing) return refreshing
    const previous = credentials
    const uid = tokenUID(previous.accessToken)
    refreshing = (async () => {
      try {
        const { response, payload } = await request('/vb/api/auth/refresh', {
          method: 'POST',
          body: {},
          cookie: `${previous.refreshCookie.name}=${previous.refreshToken}`,
        })
        const next = credentialsFromResponse(response, payload, now())
        if (tokenUID(next.accessToken) !== uid || next.user.id !== uid) {
          throw new NativeAuthError('刷新后账户发生变化，已停止连接', { code: 'account_mismatch' })
        }
        await store.saveCredentials(next)
        credentials = next
        await syncCookies(credentials)
        return emit()
      } catch (error) {
        if (error.status === 401 || error.code === 'account_mismatch') {
          credentials = null
          await store.clearCredentials()
          await clearCookies(previous)
          emit()
        }
        throw error
      } finally {
        refreshing = null
      }
    })()
    return refreshing
  }

  async function accessToken() {
    if (!credentials) throw new NativeAuthError('登录已失效，请重新登录', { status: 401, code: 'authentication_required' })
    if (!credentials.expiresAt || credentials.expiresAt <= now() + 30000) await refresh()
    return credentials.accessToken
  }

  async function ensureProfile() {
    if (!credentials) throw new NativeAuthError('未登录', { status: 401, code: 'authentication_required' })
    const uid = tokenUID(credentials.accessToken)
    const existing = store.profile(uid)
    if (existing) return validateSessionProfile(existing, uid)
    if (!device || typeof device.uuid !== 'function') throw new Error('设备参考 UUID 不可用')
    const { payload } = await request('/api/nc/ensure', {
      method: 'POST',
      bearer: await accessToken(),
      body: {
        device_uuid: await device.uuid(),
        device_name: device.name || 'desktop',
        device_type: 'desktop',
      },
    })
    const profile = validateSessionProfile(payload, uid)
    await store.saveProfile(profile)
    return profile
  }

  async function signOut({ remote = false } = {}) {
    const previous = credentials
    if (remote && previous) {
      await request('/vb/api/auth/logout', {
        method: 'POST', body: {}, bearer: await accessToken(),
      })
    }
    credentials = null
    await store.clearCredentials()
    if (previous) await clearCookies(previous)
    return emit()
  }

  async function fetchAuthConfig() {
    const { payload } = await request('/vb/api/config')
    const values = payload?.values?.vb || {}
    const methods = Array.isArray(values['auth.login.methods']) ? values['auth.login.methods'] : ['password']
    const emailEnabled = !!values['email.enabled']
    const smsEnabled = !!values['sms.enabled']
    return {
      passwordLogin: methods.includes('password'),
      emailCodeLogin: methods.includes('email_code') && emailEnabled,
      smsCodeLogin: methods.includes('phone_code') && smsEnabled,
      registration: values['auth.registration.enabled'] !== false,
      requireEmail: !!values['auth.registration.require_email'],
      requirePhone: !!values['auth.registration.require_phone'],
      emailEnabled,
      smsEnabled,
      termsURL: values['auth.legal.terms_url'] || '',
      privacyURL: values['auth.legal.privacy_url'] || '',
    }
  }

  return {
    server: base,
    restore,
    state,
    refresh,
    accessToken,
    ensureProfile,
    fetchAuthConfig,
    signOut,
    loginPassword: ({ username, password }) => authenticate('/vb/api/auth/login', { username, password }),
    loginCode: ({ type, target, code }) => authenticate('/vb/api/auth/login/code', { type, target, code }),
    register: input => authenticate('/vb/api/auth/register', input),
    sendCode: ({ type, target, purpose }) => request('/vb/api/verification/send', {
      method: 'POST', body: { type, target, purpose },
    }).then(() => ({ ok: true })),
    // 仅给 Desktop 私有 stdio RPC：不会暴露到 window.aicHost。
    sessionBootstrap: ensureProfile,
  }
}

module.exports = { createNativeAuth, NativeAuthError }
