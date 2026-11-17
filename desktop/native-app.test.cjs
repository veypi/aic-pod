'use strict'

const test = require('node:test')
const assert = require('node:assert')
const fs = require('node:fs')
const os = require('node:os')
const path = require('node:path')
const { createNativeApp } = require('./native-app.cjs')

const UID_A = '0123456789abcdef0123456789abcdef'
const UID_B = 'fedcba9876543210fedcba9876543210'

function jwt(uid, type) {
  const encode = value => Buffer.from(JSON.stringify(value)).toString('base64url')
  return `${encode({ alg: 'HS256' })}.${encode({ uid, sub: uid, type, mod: 'inherit', exp: Math.floor(Date.now() / 1000) + 1800 })}.sig`
}

function response(body, { uid = UID_A, cookies = false, status = 200 } = {}) {
  const lines = cookies ? [
    `vb_access=${jwt(uid, 'access')}; Path=/; HttpOnly; SameSite=Strict`,
    `vb_refresh=${jwt(uid, 'refresh')}; Path=/vb/api/auth/refresh; HttpOnly; SameSite=Strict`,
  ] : []
  return {
    ok: status >= 200 && status < 300,
    status,
    headers: { getSetCookie: () => lines },
    text: async () => JSON.stringify(body),
  }
}

function storage() {
  return {
    isEncryptionAvailable: () => true,
    encryptString: value => Buffer.from('encrypted\0' + value),
    decryptString: value => Buffer.from(value.toString('utf8').slice('encrypted\0'.length)),
  }
}

async function setup() {
  const userDataPath = fs.mkdtempSync(path.join(os.tmpdir(), 'aic-native-app-'))
  const backend = {
    running: false,
    connected: false,
    starts: 0,
    stops: 0,
    async start() { this.running = true; this.connected = true; this.starts++ },
    async stop() { this.running = false; this.connected = false; this.stops++ },
    async restart() { this.running = true; this.connected = true; this.starts++ },
    alive() { return this.running },
  }
  const workspaces = []
  const logins = []
  const changes = []
  let ensureCalls = 0
  const fetch = async (url, options) => {
    if (url.endsWith('/vb/api/auth/login')) {
      const request = JSON.parse(options.body)
      const uid = request.username === 'bob' ? UID_B : UID_A
      return response({ user: { id: uid, username: request.username } }, { uid, cookies: true })
    }
    if (url.endsWith('/api/nc/ensure')) {
      ensureCalls++
      const token = options.headers.Authorization.slice('Bearer '.length)
      const payload = JSON.parse(Buffer.from(token.split('.')[1], 'base64url').toString())
      const uid = payload.uid
      return response({ host_id: uid + '0001', uid, credential_ver: 1, protocol_secret: 'secret-' + uid })
    }
    if (url.endsWith('/vb/api/auth/logout')) return response({})
    throw new Error('unexpected ' + url)
  }
  const cookieJar = {
    set: async () => {}, remove: async () => {}, flushStore: async () => {},
  }
  const app = createNativeApp({
    server: 'http://localhost:4000',
    safeStorage: storage(),
    electronSession: { defaultSession: { cookies: cookieJar } },
    userDataPath,
    backend,
    fetch,
    waitConnected: async () => ({ connected: backend.connected, last_error: 'not connected' }),
    showLogin: error => { logins.push(error) },
    loadWorkspace: async () => { workspaces.push('workspace') },
    onChanged: value => { changes.push(value) },
    hostname: 'test-host',
  })
  await app.init()
  return { app, backend, workspaces, logins, changes, userDataPath, ensureCalls: () => ensureCalls }
}

test('登录自动 ensure 并等 Host 连接后进入工作区，切回账户复用原 Host 档案', async () => {
  const ctx = await setup()
  const first = await ctx.app.loginPassword({ username: 'alice', password: 'secret' })
  assert.equal(first.active, true)
  assert.equal(first.host_id, UID_A + '0001')
  assert.equal(ctx.backend.starts, 1)
  assert.equal(ctx.workspaces.length, 1)
  assert.equal(ctx.ensureCalls(), 1)
  const rpcToken = await ctx.app.sessionRPC.accessToken()
  assert.match(rpcToken.access_token, /^eyJ/)

  await ctx.app.switchAccount()
  assert.equal(ctx.app.state().signedIn, false)
  assert.equal(ctx.backend.running, false)

  const second = await ctx.app.loginPassword({ username: 'bob', password: 'secret' })
  assert.equal(second.host_id, UID_B + '0001')
  assert.notEqual(second.host_id, first.host_id)
  assert.equal(ctx.ensureCalls(), 2)

  await ctx.app.switchAccount()
  await ctx.app.loginPassword({ username: 'alice', password: 'new-secret' })
  assert.equal(ctx.app.state().host_id, UID_A + '0001')
  assert.equal(ctx.ensureCalls(), 2, '切回 A 不应重复 ensure')
  assert.equal(ctx.workspaces.length, 3)
})

test('连接失败不进入工作区，登出完整停止 Pod 并回原生登录页', async () => {
  const ctx = await setup()
  ctx.backend.start = async () => { ctx.backend.running = true; ctx.backend.connected = false; ctx.backend.starts++ }
  await assert.rejects(() => ctx.app.loginPassword({ username: 'alice', password: 'secret' }), /not connected/)
  assert.equal(ctx.workspaces.length, 0)
  assert.equal(ctx.app.state().active, false)
  assert.ok(ctx.logins.length > 0)

  ctx.backend.start = async () => { ctx.backend.running = true; ctx.backend.connected = true; ctx.backend.starts++ }
  await ctx.app.loginPassword({ username: 'alice', password: 'secret' })
  const beforeStop = ctx.backend.stops
  await ctx.app.signOut({ remote: true })
  assert.equal(ctx.app.state().signedIn, false)
  assert.equal(ctx.backend.running, false)
  assert.ok(ctx.backend.stops > beforeStop)
})
