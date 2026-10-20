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

test('并发 start+stop 串行化：start 等 stop 完成后再拉起', async () => {
  const children = [fakeChild(), fakeChild()]
  let spawnCount = 0
  const order = []
  const ctl = createBackend({
    spawn: () => {
      spawnCount++
      order.push('spawn')
      return children[spawnCount - 1]
    },
    binary: 'aic-backend',
    env: {},
    waitForStartup: async () => {},
    onFatal: () => {},
  })
  await ctl.start()
  // stop 的 kill 不立即触发 close（模拟慢退出）：start 必须排队等它
  const slow = children[0]
  slow.kill = () => { order.push('kill'); setTimeout(() => { slow.signalCode = 'SIGTERM'; slow.emit('close', null, 'SIGTERM') }, 20) }
  const stopping = ctl.stop()
  const starting = ctl.start() // 并发到达：应排在 stop 之后
  await Promise.all([stopping, starting])
  assert.deepEqual(order, ['spawn', 'kill', 'spawn'])
  assert.equal(spawnCount, 2)
  assert.equal(ctl.alive(), true)
  assert.equal(ctl.child, children[1])
})
