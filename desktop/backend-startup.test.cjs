const test = require('node:test')
const assert = require('node:assert/strict')
const fs = require('node:fs')
const os = require('node:os')
const path = require('node:path')
const { EventEmitter } = require('node:events')
const { PassThrough } = require('node:stream')
const { spawn } = require('node:child_process')
const { waitForBackend } = require('./backend-startup.cjs')

function fixture(t) {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'aic-startup-'))
  const portFile = path.join(directory, 'port.json')
  const child = new EventEmitter()
  child.stdout = new PassThrough()
  child.stderr = new PassThrough()
  t.after(() => fs.rmSync(directory, { recursive: true, force: true }))
  return { child, portFile }
}

function assertClean(child) {
  for (const event of ['error', 'exit', 'close']) assert.equal(child.listenerCount(event), 0)
  assert.equal(child.stdout.listenerCount('data'), 0)
  assert.equal(child.stderr.listenerCount('data'), 0)
}

test('waits for a complete handshake and removes startup listeners', async (t) => {
  const { child, portFile } = fixture(t)
  fs.writeFileSync(portFile, '{"port":')
  const ready = waitForBackend(child, portFile, { pollMs: 5, timeoutMs: 1000 })
  const info = { port: 12345, code: 'local-secret' }
  fs.writeFileSync(portFile, JSON.stringify(info))
  assert.deepEqual(await ready, info)
  assertClean(child)
})

test('rejects an empty code or invalid port immediately', async (t) => {
  for (const info of [null, {}, { port: 12345, code: '' }, { port: 12345, code: ' ' },
    { port: 0, code: 'x' }, { port: 65536, code: 'x' }, { port: '12345', code: 'x' }]) {
    const { child, portFile } = fixture(t)
    fs.writeFileSync(portFile, JSON.stringify(info))
    await assert.rejects(waitForBackend(child, portFile), /握手无效/)
    assertClean(child)
  }
})

test('reports a real child startup failure instead of timing out', async (t) => {
  const { portFile } = fixture(t)
  const child = spawn(process.execPath, ['-e', 'process.stderr.write("local API: address unavailable\\n"); process.exitCode = 1'], { stdio: ['ignore', 'pipe', 'pipe'] })
  t.after(() => child.kill())
  await assert.rejects(waitForBackend(child, portFile), (error) => {
    assert.match(error.message, /启动完成前退出（1）/)
    assert.match(error.message, /local API: address unavailable/)
    return true
  })
})

test('reports a missing backend executable without an unhandled error', async (t) => {
  const { portFile } = fixture(t)
  const child = spawn(path.join(path.dirname(portFile), 'missing-backend'))
  await assert.rejects(waitForBackend(child, portFile), /无法启动本地服务.*ENOENT/)
})

test('drains final stderr on exit and does not accept a dead backend handshake', async (t) => {
  const { child, portFile } = fixture(t)
  const ready = waitForBackend(child, portFile)
  child.emit('exit', 1, null)
  fs.writeFileSync(portFile, JSON.stringify({ port: 12345, code: 'x' }))
  child.stderr.write('\u001b[31mfinal startup error\u001b[0m')
  child.emit('close', 1, null)
  await assert.rejects(ready, (error) => {
    assert.match(error.message, /final startup error/)
    assert.ok(!error.message.includes('\u001b'))
    return true
  })
  assertClean(child)
})

test('times out with bounded diagnostics even when a child exits without closing pipes', async (t) => {
  const { child, portFile } = fixture(t)
  const ready = waitForBackend(child, portFile, { timeoutMs: 20, pollMs: 5 })
  child.stderr.write('x'.repeat(16000) + 'last error')
  child.emit('exit', 1, null)
  await assert.rejects(ready, (error) => {
    assert.match(error.message, /启动超时/)
    assert.match(error.message, /last error$/)
    assert.ok(error.message.length < 8500)
    return true
  })
  assertClean(child)
})

test('times out when no handshake arrives', async (t) => {
  const { child, portFile } = fixture(t)
  await assert.rejects(waitForBackend(child, portFile, { timeoutMs: 20, pollMs: 5 }), /启动超时/)
  assertClean(child)
})
