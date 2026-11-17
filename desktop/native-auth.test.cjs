'use strict'

const test = require('node:test')
const assert = require('node:assert')
const fs = require('node:fs')
const os = require('node:os')
const path = require('node:path')
const { createNativeAuth } = require('./native-auth.cjs')
const { createAuthStore } = require('./auth-store.cjs')
const { credentialsFromResponse, credentialsFromCookies, validateSessionProfile } = require('./auth-cookies.cjs')

const UID_A = '0123456789abcdef0123456789abcdef'
const UID_B = 'fedcba9876543210fedcba9876543210'

function jwt(uid, type, exp) {
  const encode = value => Buffer.from(JSON.stringify(value)).toString('base64url')
  return `${encode({ alg: 'HS256', typ: 'JWT' })}.${encode({ uid, sub: uid, type, mod: 'inherit', exp })}.sig`
}

function cookie(name, value, cookiePath) {
  return `${name}=${value}; Path=${cookiePath}; HttpOnly; Secure; SameSite=Strict`
}

function response({ status = 200, body = {}, uid = UID_A, exp = Math.floor(Date.now() / 1000) + 900, cookies = true } = {}) {
  const access = jwt(uid, 'access', exp)
  const refresh = jwt(uid, 'refresh', exp + 86400)
  const lines = cookies
    ? [cookie('vb_access', access, '/'), cookie('vb_refresh', refresh, '/vb/api/auth/refresh')]
    : []
  return {
    ok: status >= 200 && status < 300,
    status,
    headers: { getSetCookie: () => lines },
    text: async () => JSON.stringify(body),
  }
}

function loginResponse(uid = UID_A, exp) {
  return response({
    uid,
    exp,
    body: { expires_in: 900, user: { id: uid, username: 'alice', nickname: 'Alice' } },
  })
}

function fakeStorage() {
  return {
    isEncryptionAvailable: () => true,
    encryptString: value => Buffer.from('encrypted\0' + value),
    decryptString: value => Buffer.from(value.toString('utf8').slice('encrypted\0'.length)),
  }
}

async function setup(fetchImpl, now = () => Date.now()) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'aic-native-auth-'))
  const store = createAuthStore({ safeStorage: fakeStorage(), file: path.join(dir, 'session.json') })
  await store.init()
  const synced = []
  const cleared = []
  const auth = createNativeAuth({
    server: 'http://localhost:4000',
    store,
    fetch: fetchImpl,
    now,
    device: { uuid: async () => '3d0ca0f1-62de-4ed2-9a93-3bfa4970e734', name: 'test-desktop' },
    syncCookies: async value => { synced.push(value) },
    clearCookies: async value => { cleared.push(value) },
  })
  return { auth, store, dir, synced, cleared }
}

test('三方授权地址由服务端签发，adopt 接管会话并广播登录态', async () => {
  const exp = Math.floor(Date.now() / 1000) + 900
  const calls = []
  const { auth, store, synced } = await setup(async (url, options) => {
    calls.push({ url, options })
    if (url.includes('/vb/api/auth/authorize/thirdparty')) {
      return response({ body: { auth_url: 'https://accounts.google.com/o/oauth2/v2/auth?client_id=x' }, cookies: false })
    }
    throw new Error('unexpected request ' + url)
  })
  const authUrl = await auth.authorizeThirdParty('google', 'http://localhost:4000/u')
  assert.match(authUrl, /^https:\/\/accounts\.google\.com\//)
  const authorize = calls.find(call => call.url.includes('/authorize/thirdparty'))
  assert.match(authorize.url, /provider=google/)
  assert.match(authorize.url, /redirect=http%3A%2F%2Flocalhost%3A4000%2Fu/)
  await assert.rejects(() => auth.authorizeThirdParty(''), /未指定第三方登录提供商/)

  const next = credentialsFromCookies([
    { name: 'vb_access', value: jwt(UID_A, 'access', exp), secure: true, httpOnly: true },
    { name: 'vb_refresh', value: jwt(UID_A, 'refresh', exp + 86400), secure: true, httpOnly: true },
  ], Date.now())
  assert.ok(next)
  const state = await auth.adopt(next)
  assert.equal(state.signedIn, true)
  assert.equal(state.user.id, UID_A)
  assert.equal(synced.length, 1)
  assert.equal(store.current().user.id, UID_A)
})

test('会话 cookie 重建凭据：两个 token 必须同属一个用户', () => {
  const exp = Math.floor(Date.now() / 1000) + 900
  const next = credentialsFromCookies([
    { name: 'vb_access', value: jwt(UID_A, 'access', exp), secure: true, httpOnly: true },
    { name: 'vb_refresh', value: jwt(UID_A, 'refresh', exp + 86400), secure: true, httpOnly: true },
  ], Date.now())
  assert.ok(next)
  assert.equal(next.user.id, UID_A)
  assert.equal(next.accessCookie.name, 'vb_access')
  assert.equal(next.refreshCookie.name, 'vb_refresh')
  const mixed = credentialsFromCookies([
    { name: 'vb_access', value: jwt(UID_A, 'access', exp) },
    { name: 'vb_refresh', value: jwt(UID_B, 'refresh', exp) },
  ], Date.now())
  assert.equal(mixed, null)
  assert.equal(credentialsFromCookies([], Date.now()), null)
})

test('登录只从 Set-Cookie 取 token，ensure 使用原始 Bearer 并保存完整 Host 档案', async () => {
  const calls = []
  const { auth, store, synced } = await setup(async (url, options) => {
    calls.push({ url, options })
    if (url.endsWith('/vb/api/auth/login')) return loginResponse()
    if (url.endsWith('/api/nc/ensure')) {
      return response({
        body: {
          host_id: UID_A + '0007', host_ref: '0007', uid: UID_A,
          credential_ver: 2, protocol_secret: 'host-protocol-secret',
        },
        cookies: false,
      })
    }
    throw new Error('unexpected request ' + url)
  })
  const state = await auth.loginPassword({ username: 'alice', password: 'secret' })
  assert.equal(state.signedIn, true)
  assert.equal(Object.hasOwn(state, 'accessToken'), false)
  const profile = await auth.ensureProfile()
  assert.equal(profile.host_id, UID_A + '0007')
  assert.equal(profile.credential_ver, 2)
  const ensure = calls.find(call => call.url.endsWith('/api/nc/ensure'))
  assert.match(ensure.options.headers.Authorization, /^Bearer eyJ/)
  assert.doesNotMatch(ensure.options.headers.Authorization, /^Bearer s1\./)
  assert.equal(JSON.parse(ensure.options.body).device_type, 'desktop')
  assert.equal(synced.length, 1)
  assert.deepEqual((await auth.sessionBootstrap()).host_id, UID_A + '0007')
  assert.equal(store.profile(UID_A).protocol_secret, 'host-protocol-secret')
})

test('过期 access token 的并发读取共用一次原生刷新，Cookie 同步一次', async () => {
  let now = 1_800_000_000_000
  let refreshCalls = 0
  let release
  const gate = new Promise(resolve => { release = resolve })
  const { auth, synced } = await setup(async url => {
    if (url.endsWith('/vb/api/auth/login')) return loginResponse(UID_A, Math.floor(now / 1000) + 1)
    if (url.endsWith('/vb/api/auth/refresh')) {
      refreshCalls++
      await gate
      return loginResponse(UID_A, Math.floor(now / 1000) + 1800)
    }
    throw new Error('unexpected ' + url)
  }, () => now)
  await auth.loginPassword({ username: 'alice', password: 'secret' })
  now += 2000
  const first = auth.accessToken()
  const second = auth.accessToken()
  await Promise.resolve()
  await Promise.resolve()
  release()
  const tokens = await Promise.all([first, second])
  assert.equal(refreshCalls, 1)
  assert.equal(tokens[0], tokens[1])
  assert.equal(synced.length, 2)
})

test('refresh 失效清除 token 与 Cookie，但保留原账户 Host 档案', async () => {
  let now = 1_800_000_000_000
  const { auth, store, cleared } = await setup(async url => {
    if (url.endsWith('/vb/api/auth/login')) return loginResponse(UID_A, Math.floor(now / 1000) + 1800)
    if (url.endsWith('/api/nc/ensure')) return response({
      body: { host_id: UID_A + '0001', uid: UID_A, credential_ver: 1, protocol_secret: 'secret' }, cookies: false,
    })
    if (url.endsWith('/vb/api/auth/refresh')) return response({ status: 401, body: { message: 'invalid token' }, cookies: false })
    throw new Error('unexpected ' + url)
  }, () => now)
  await auth.loginPassword({ username: 'alice', password: 'secret' })
  await auth.ensureProfile()
  await assert.rejects(() => auth.refresh(), /invalid token/)
  assert.equal(auth.state().signedIn, false)
  assert.equal(store.current(), null)
  assert.equal(store.profile(UID_A).host_id, UID_A + '0001')
  assert.equal(cleared.length, 1)
})

test('登录失败携带平台业务码（凭据错误 40104）', async () => {
  const { auth } = await setup(async url => {
    if (url.endsWith('/vb/api/auth/login')) {
      return response({ status: 401, body: { code: 40104, message: 'invalid credentials' }, cookies: false })
    }
    throw new Error('unexpected ' + url)
  })
  await assert.rejects(
    () => auth.loginPassword({ username: 'admin', password: 'wrong' }),
    (error) => {
      assert.equal(error.serverCode, 40104)
      assert.equal(error.message, 'invalid credentials')
      assert.equal(error.status, 401)
      return true
    },
  )
})

test('刷新后 uid 变化必须 fail closed，不允许旧 Host 漂移账户', async () => {
  const { auth, store } = await setup(async url => {
    if (url.endsWith('/vb/api/auth/login')) return loginResponse(UID_A)
    if (url.endsWith('/vb/api/auth/refresh')) return loginResponse(UID_B)
    throw new Error('unexpected ' + url)
  })
  await auth.loginPassword({ username: 'alice', password: 'secret' })
  await assert.rejects(() => auth.refresh(), /账户发生变化/)
  assert.equal(auth.state().signedIn, false)
  assert.equal(store.current(), null)
})

test('加密档案落盘不含明文 token/secret，登出后仍可恢复 Host profile', async () => {
  const { auth, store, dir } = await setup(async url => {
    if (url.endsWith('/vb/api/auth/login')) return loginResponse()
    if (url.endsWith('/api/nc/ensure')) return response({
      body: { host_id: UID_A + '0002', uid: UID_A, credential_ver: 3, protocol_secret: 'plaintext-host-secret' }, cookies: false,
    })
    throw new Error('unexpected ' + url)
  })
  await auth.loginPassword({ username: 'alice', password: 'secret' })
  await auth.ensureProfile()
  const raw = fs.readFileSync(path.join(dir, 'session.json'), 'utf8')
  assert.doesNotMatch(raw, /plaintext-host-secret/)
  assert.doesNotMatch(raw, /eyJhbGci/)
  await auth.signOut()
  assert.equal(store.current(), null)
  assert.equal(store.profile(UID_A).host_id, UID_A + '0002')
})

test('Cookie 解析与 Host 档案拒绝错误账户和短 Host ID', () => {
  const parsed = credentialsFromResponse(loginResponse(), { user: { id: UID_A } })
  assert.equal(parsed.user.id, UID_A)
  assert.equal(parsed.refreshCookie.path, '/vb/api/auth/refresh')
  assert.throws(() => validateSessionProfile({ host_id: '0001', uid: UID_A, credential_ver: 1, protocol_secret: 'x' }, UID_A), /Host ID/)
  assert.throws(() => validateSessionProfile({ host_id: UID_B + '0001', uid: UID_B, credential_ver: 1, protocol_secret: 'x' }, UID_A), /不一致/)
})
