const test = require('node:test')
const assert = require('node:assert')
const { EventEmitter } = require('node:events')
const { createBackend } = require('./backend.cjs')

function fakeChild() {
  const child = new EventEmitter()
  child.stdout = new EventEmitter()
  child.stderr = new EventEmitter()
  child.exitCode = null
  child.signalCode = null
  child.killed = false
  child.kill = (signal) => {
    child.killed = true
    child.signalCode = signal
    child.emit('close', null, signal)
  }
  return child
}

test('start/stop 与就绪状态', async () => {
  let child = fakeChild()
  let fatal = 0
  const ctl = createBackend({
    spawn: () => child,
    binary: 'aic-backend',
    env: {},
    waitForStartup: async () => {},
    onFatal: () => { fatal++ },
  })
  await ctl.start()
  assert.equal(ctl.alive(), true)
  child.exitCode = 0
  child.emit('exit', 0, null) // 就绪后异常退出 → onFatal
  assert.equal(fatal, 1)
  assert.equal(ctl.alive(), false)

  child = fakeChild()
  await ctl.start()
  await ctl.stop()
  assert.equal(ctl.alive(), false)
  assert.equal(child.killed, true)
})

test('主动 stop 不算启动失败', async () => {
  const child = fakeChild()
  let fatal = 0
  const ctl = createBackend({
    spawn: () => child,
    binary: 'aic-backend',
    env: {},
    waitForStartup: async () => {},
    onFatal: () => { fatal++ },
  })
  await ctl.start()
  await ctl.stop()
  child.emit('exit', 1, 'SIGTERM')
  assert.equal(fatal, 0)
})
