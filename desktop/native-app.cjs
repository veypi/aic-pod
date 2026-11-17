'use strict'

const crypto = require('node:crypto')
const path = require('node:path')
const { createAuthStore } = require('./auth-store.cjs')
const { createNativeAuth } = require('./native-auth.cjs')
const { createElectronCookies } = require('./electron-cookies.cjs')
const { createDeviceIdentity } = require('./device-identity.cjs')
const { normalizeServer } = require('./auth-cookies.cjs')

function createNativeApp({
  server,
  safeStorage,
  electronSession,
  userDataPath,
  backend,
  waitConnected,
  showLogin,
  loadWorkspace,
  onChanged = () => {},
  fetch: fetchImpl,
  hostname = 'desktop',
}) {
  const base = normalizeServer(server)
  const serverKey = crypto.createHash('sha256').update(base).digest('hex').slice(0, 32)
  const store = createAuthStore({
    safeStorage,
    file: path.join(userDataPath, 'native-auth', serverKey + '.json'),
  })
  const cookies = createElectronCookies({ server: base, session: electronSession })
  const device = createDeviceIdentity({ file: path.join(userDataPath, 'device.uuid'), name: hostname })
  let active = false
  let activating = null
  let lastError = ''
  let booted = false
  let authConfig = null

  const auth = createNativeAuth({
    server: base,
    fetch: fetchImpl,
    store,
    syncCookies: cookies.sync,
    clearCookies: cookies.clear,
    device,
    onChanged: value => {
      onChanged(value)
      if (!value.signedIn) {
        active = false
        Promise.resolve(backend.stop()).catch(() => {}).then(() => showLogin(''))
      }
    },
  })

  function currentProfile() {
    return store.profile()
  }

  function state() {
    return {
      ...auth.state(),
      active,
      error: lastError,
      host_id: currentProfile()?.host_id || '',
      server: base,
    }
  }

  async function activate({ openWorkspace = true } = {}) {
    if (activating) return activating
    activating = (async () => {
      lastError = ''
      try {
        await auth.ensureProfile()
        if (!backend.alive()) {
          await backend.start()
        } else {
          const current = await waitConnected(0)
          if (!current?.connected) await backend.restart()
        }
        const status = await waitConnected(15000)
        if (!status?.connected) {
          await backend.stop()
          throw new Error(status?.last_error || 'Host 尚未连接，请检查网络与登录状态')
        }
        active = true
        if (openWorkspace) await loadWorkspace()
        return state()
      } catch (error) {
        active = false
        lastError = error.message || String(error)
        await backend.stop().catch(() => {})
        showLogin(lastError)
        throw error
      } finally {
        activating = null
      }
    })()
    return activating
  }

  async function init() {
    await store.init()
  }

  async function boot() {
    booted = true
    const current = await auth.restore()
    if (!current.signedIn) {
      showLogin('')
      return state()
    }
    try {
      return await activate()
    } catch (_) {
      return state()
    }
  }

  async function login(method, input) {
    if (backend.alive()) await backend.stop()
    active = false
    if (auth.state().signedIn) await auth.signOut()
    if (!['loginPassword', 'loginCode', 'register'].includes(method)) throw new Error('非法登录方法')
    await auth[method](input)
    return activate()
  }

  async function switchAccount() {
    active = false
    lastError = ''
    await backend.stop()
    await auth.signOut()
    showLogin('')
    return state()
  }

  async function signOut({ remote = false } = {}) {
    lastError = ''
    await auth.signOut({ remote })
    active = false
    await backend.stop()
    showLogin('')
    return { ok: true }
  }

  async function openLogin() {
    active = false
    await backend.stop()
    showLogin('')
    return { ok: true }
  }

  async function stop() {
    active = false
    await backend.stop()
    return state()
  }

  async function refresh() {
    return auth.refresh()
  }

  async function restart() {
    if (!auth.state().signedIn) {
      await backend.stop()
      return state()
    }
    await auth.ensureProfile()
    if (backend.alive()) await backend.restart()
    else return activate({ openWorkspace: false })
    const status = await waitConnected(20000)
    if (!status?.connected) throw new Error(status?.last_error || 'Host 重连失败')
    active = true
    return state()
  }

  function handleBackendFatal(error) {
    active = false
    lastError = error?.message || String(error)
    showLogin(lastError)
  }

  return {
    server: base,
    init,
    boot,
    state,
    currentProfile,
    activate,
    restart,
    refresh,
    signOut,
    openLogin,
    stop,
    switchAccount,
    loginPassword: input => login('loginPassword', input),
    loginCode: input => login('loginCode', input),
    register: input => login('register', input),
    sendCode: input => auth.sendCode(input),
    fetchAuthConfig: () => {
      if (!authConfig) {
        authConfig = auth.fetchAuthConfig().catch(error => {
          authConfig = null
          throw error
        })
      }
      return authConfig
    },
    handleBackendFatal,
    sessionRPC: {
      bootstrap: () => auth.sessionBootstrap(),
      accessToken: () => auth.accessToken().then(access_token => ({ access_token })),
    },
    get booted() { return booted },
  }
}

module.exports = { createNativeApp }
