'use strict'

const test = require('node:test')
const assert = require('node:assert')
const { EventEmitter } = require('node:events')
const { PassThrough } = require('node:stream')
const { attachSessionRPC, safeError } = require('./session-rpc.cjs')

function fakeChild() {
  const child = new EventEmitter()
  child.stdout = new PassThrough()
  child.stdin = new PassThrough()
  child.stdin.destroyed = false
  child.killed = false
  return child
}

function oneFrame(child) {
  return new Promise(resolve => {
    let data = ''
    child.stdin.once('data', chunk => {
      data += chunk.toString()
      resolve(JSON.parse(data))
    })
  })
}

test('stdio RPC 分别返回 Host 档案和每次最新 access token', async () => {
  const child = fakeChild()
  let token = 'token-a'
  const rpc = attachSessionRPC(child, {
    bootstrap: async () => ({ host_id: 'a'.repeat(32) + '0001', protocol_secret: 'secret' }),
    accessToken: async () => ({ access_token: token }),
  })
  let response = oneFrame(child)
  child.stdout.write(JSON.stringify({ id: '1', method: 'session_bootstrap' }) + '\n')
  assert.equal((await response).result.protocol_secret, 'secret')
  response = oneFrame(child)
  child.stdout.write(JSON.stringify({ id: '2', method: 'access_token' }) + '\n')
  assert.equal((await response).result.access_token, 'token-a')
  token = 'token-b'
  response = oneFrame(child)
  child.stdout.write(JSON.stringify({ id: '3', method: 'access_token' }) + '\n')
  assert.equal((await response).result.access_token, 'token-b')
  rpc.close()
  child.stdout.end()
})

test('非法 stdout 日志行显式报协议错误，错误回执不泄漏 JWT', async () => {
  const child = fakeChild()
  const errors = []
  const rpc = attachSessionRPC(child, {
    bootstrap: async () => { throw new Error('failed eyJhbGciOiJIUzI1NiJ9.eyJ1aWQiOiJ4In0.c2ln') },
    accessToken: async () => ({}),
    onError: error => errors.push(error),
  })
  child.stdout.write('console log must not mix with rpc\n')
  await new Promise(resolve => setImmediate(resolve))
  assert.equal(errors.length, 1)
  assert.match(errors[0].message, /非法控制帧/)
  const pending = oneFrame(child)
  child.stdout.write(JSON.stringify({ id: 'x', method: 'session_bootstrap' }) + '\n')
  const response = await pending
  assert.doesNotMatch(response.error, /eyJhbGci/)
  assert.match(response.error, /redacted jwt/)
  rpc.close()
  child.stdout.end()
})

test('safeError 同时清理 s1 envelope', () => {
  const value = safeError(new Error('bad s1.' + 'a'.repeat(36) + '.2.header.payload.sig'))
  assert.doesNotMatch(value, /s1\./)
})
