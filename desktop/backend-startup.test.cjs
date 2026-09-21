// waitForStartup 单测（node --test）：就绪判定 = graceMs 窗口内未退出/未报错。
const test = require('node:test')
const assert = require('node:assert')
const { spawn } = require('node:child_process')
const { waitForStartup } = require('./backend-startup.cjs')

const NODE = process.execPath

test('窗口内未退出 → 就绪', async () => {
  const child = spawn(NODE, ['-e', 'setTimeout(() => {}, 3000)'], { stdio: ['ignore', 'pipe', 'pipe'] })
  await waitForStartup(child, { graceMs: 150 })
  assert.equal(child.exitCode, null)
  child.kill('SIGKILL')
})

test('提前退出 → 报错并携带日志尾部', async () => {
  const child = spawn(NODE, ['-e', "console.error('\\u001b[31mboom: invalid credential\\u001b[0m'); process.exit(3)"], { stdio: ['ignore', 'pipe', 'pipe'] })
  await assert.rejects(() => waitForStartup(child, { graceMs: 5000 }), (err) => {
    assert.match(err.message, /启动完成前退出/)
    assert.match(err.message, /boom: invalid credential/)
    assert.doesNotMatch(err.message, /\u001b\[/) // 控制字符已剥离
    return true
  })
})

test('无法 spawn（二进制缺失）→ 报错', async () => {
  const child = spawn('/nonexistent/aic-backend', [], { stdio: ['ignore', 'pipe', 'pipe'] })
  await assert.rejects(() => waitForStartup(child, { graceMs: 5000 }), /无法启动本地服务/)
})
