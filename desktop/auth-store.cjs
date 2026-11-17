'use strict'

const fs = require('node:fs')
const path = require('node:path')

// 每台服务器一个加密档案：accounts 按 uid 隔离 Host profile；只有 currentUID
// 指向的账户持有 Web token。登出删除 token，但保留该账户的 Host profile 供下次复用。
function createAuthStore({ safeStorage, file }) {
  let state = { version: 1, currentUID: '', accounts: {} }
  let queue = Promise.resolve()

  async function init() {
    if (!safeStorage || typeof safeStorage.encryptString !== 'function' || typeof safeStorage.decryptString !== 'function') {
      throw new Error('系统安全存储不可用')
    }
    if (typeof safeStorage.isEncryptionAvailable === 'function' && !safeStorage.isEncryptionAvailable()) {
      throw new Error('系统安全存储未就绪，无法保存登录状态')
    }
    try {
      const envelope = JSON.parse(await fs.promises.readFile(file, 'utf8'))
      if (envelope?.version !== 1 || typeof envelope.data !== 'string') throw new Error('登录档案格式无效')
      const decoded = JSON.parse(safeStorage.decryptString(Buffer.from(envelope.data, 'base64')).toString('utf8'))
      if (decoded?.version !== 1 || typeof decoded.accounts !== 'object' || decoded.accounts === null) {
        throw new Error('登录档案内容无效')
      }
      state = {
        version: 1,
        currentUID: typeof decoded.currentUID === 'string' ? decoded.currentUID : '',
        accounts: decoded.accounts,
      }
      if (state.currentUID && !state.accounts[state.currentUID]?.credentials) state.currentUID = ''
    } catch (error) {
      if (error.code === 'ENOENT') return
      throw new Error('无法读取加密登录档案: ' + error.message)
    }
  }

  function snapshot() {
    return structuredClone(state)
  }

  function current() {
    const account = state.accounts[state.currentUID]
    return account?.credentials ? structuredClone(account.credentials) : null
  }

  function profile(uid = state.currentUID) {
    const value = state.accounts[uid]?.profile
    return value ? structuredClone(value) : null
  }

  function commit(mutator) {
    const run = queue.then(async () => {
      const next = structuredClone(state)
      const result = mutator(next)
      const plaintext = JSON.stringify(next)
      const envelope = JSON.stringify({ version: 1, data: safeStorage.encryptString(plaintext).toString('base64') })
      await fs.promises.mkdir(path.dirname(file), { recursive: true, mode: 0o700 })
      const temp = file + '.tmp-' + process.pid + '-' + Math.random().toString(16).slice(2)
      try {
        await fs.promises.writeFile(temp, envelope, { mode: 0o600 })
        await fs.promises.rename(temp, file)
      } finally {
        await fs.promises.rm(temp, { force: true }).catch(() => {})
      }
      state = next
      return result
    })
    queue = run.then(() => {}, () => {})
    return run
  }

  async function saveLogin(credentials) {
    const uid = credentials?.user?.id
    if (!/^[0-9a-f]{32}$/.test(String(uid || ''))) throw new Error('登录响应缺少有效用户 ID')
    await commit(next => {
      const previousUID = next.currentUID
      if (previousUID && previousUID !== uid && next.accounts[previousUID]) {
        delete next.accounts[previousUID].credentials
      }
      const previous = next.accounts[uid] || {}
      next.accounts[uid] = { ...previous, credentials: structuredClone(credentials) }
      next.currentUID = uid
    })
  }

  async function saveCredentials(credentials) {
    const uid = state.currentUID
    if (!uid || credentials?.user?.id !== uid) throw new Error('不能跨账户更新登录凭证')
    await commit(next => {
      next.accounts[uid].credentials = structuredClone(credentials)
    })
  }

  async function saveProfile(value) {
    const uid = state.currentUID
    if (!uid || value?.uid !== uid) throw new Error('不能跨账户保存 Host 档案')
    await commit(next => {
      const account = next.accounts[uid] || {}
      next.accounts[uid] = { ...account, profile: structuredClone(value) }
    })
  }

  async function clearCredentials() {
    const uid = state.currentUID
    if (!uid) return
    await commit(next => {
      if (next.accounts[uid]) delete next.accounts[uid].credentials
      next.currentUID = ''
    })
  }

  return { init, snapshot, current, profile, saveLogin, saveCredentials, saveProfile, clearCredentials }
}

module.exports = { createAuthStore }
