const test = require('node:test')
const assert = require('node:assert')
const { composeLocalStatus } = require('./host-state.cjs')

const KEY = 'aaaa1111.2.x.uid'
// 现行复合凭证（device-key-migration 2026-10-12）：<host_id>.<vbase 四段 credential>，共 5 段。
const KEY5 = 'aaaa1111.hh.pp.ss.clientsecret'

test('state 缺失/陈旧 pid 不得冒充已连接（重连假成功回归）', () => {
  let s = composeLocalStatus({ alive: true, key: KEY, childPid: 100, state: null, hostname: 'h' })
  assert.equal(s.running, true)
  assert.equal(s.connected, false)
  assert.equal(s.host_id, 'aaaa1111')
  s = composeLocalStatus({ alive: true, key: KEY, childPid: 100, state: { pid: 99, connected: true }, hostname: 'h' })
  assert.equal(s.connected, false)
  assert.equal(s.host_id, 'aaaa1111')
})

test('5 段复合凭证同样能兑底 host_id（vbase 迁移回归）', () => {
  const s = composeLocalStatus({ alive: true, key: KEY5, childPid: 100, state: null, hostname: 'h' })
  assert.equal(s.host_id, 'aaaa1111')
})

test('形态不合法的凭证不兑底 host_id（半截串不冒充已绑定设备）', () => {
  for (const key of ['aaaa1111.2', 'aaaa1111', 'aaaa1111.2.x.uid.extra.more']) {
    const s = composeLocalStatus({ alive: true, key, childPid: 100, state: null, hostname: 'h' })
    assert.equal(s.host_id, '', `key ${key} must not bind a host id`)
  }
})

test('pid 匹配时以 state 为准（真实连接 + 失败原因透出）', () => {
  let s = composeLocalStatus({
    alive: true, key: KEY, childPid: 100,
    state: { pid: 100, connected: true, host_id: 'bbbb2222', version: 'v9' }, hostname: 'h',
  })
  assert.equal(s.connected, true)
  assert.equal(s.host_id, 'bbbb2222')
  assert.equal(s.version, 'v9')
  s = composeLocalStatus({
    alive: true, key: KEY, childPid: 100,
    state: { pid: 100, connected: false, last_error: 'nats connect: nats: Authorization Violation', retrying: true }, hostname: 'h',
  })
  assert.equal(s.connected, false)
  assert.equal(s.retrying, true)
  assert.match(s.last_error, /Authorization Violation/)
})

test('进程不存活 → running/connected 恒 false（state 再新也不算）', () => {
  const s = composeLocalStatus({ alive: false, key: KEY, childPid: null, state: { pid: 1, connected: true }, hostname: 'h' })
  assert.equal(s.running, false)
  assert.equal(s.connected, false)
  assert.equal(s.retrying, false)
})
