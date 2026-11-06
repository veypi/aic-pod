const test = require('node:test')
const assert = require('node:assert')
const { isLoopbackHost, insecureOriginFor, hostFromConfigOutput } = require('./secure-origin.cjs')

test('明文 http + 非 loopback 主机需要提升为安全上下文', () => {
  assert.equal(insecureOriginFor('http://192.168.31.3:4002'), 'http://192.168.31.3:4002')
  assert.equal(insecureOriginFor('http://box.local:4000'), 'http://box.local:4000')
  assert.equal(insecureOriginFor('http://10.0.0.7'), 'http://10.0.0.7')
})

test('https 与 loopback 本就安全，不提升', () => {
  assert.equal(insecureOriginFor('https://ivec-ai.com'), '')
  assert.equal(insecureOriginFor('http://localhost:4002'), '')
  assert.equal(insecureOriginFor('http://127.0.0.1:4000'), '')
  assert.equal(insecureOriginFor('http://[::1]:4000'), '')
  assert.equal(insecureOriginFor('http://app.localhost:4000'), '')
})

test('空值与非法输入不提升', () => {
  assert.equal(insecureOriginFor(''), '')
  assert.equal(insecureOriginFor(null), '')
  assert.equal(insecureOriginFor('192.168.31.3:4002'), '')
  assert.equal(insecureOriginFor('not a url'), '')
})

test('loopback 判定覆盖 127.0.0.0/8 与 ::1，不误伤普通内网段', () => {
  assert.equal(isLoopbackHost('127.0.0.1'), true)
  assert.equal(isLoopbackHost('127.1.2.3'), true)
  assert.equal(isLoopbackHost('::1'), true)
  assert.equal(isLoopbackHost('localhost'), true)
  assert.equal(isLoopbackHost('192.168.31.3'), false)
  assert.equal(isLoopbackHost('10.0.0.7'), false)
})

test('从 config get 的 stdout 取 host：日志行（带 ANSI）在前也不误取', () => {
  // 实测输出形态：logv 的 console 行走 stdout 且带 ANSI 颜色，JSON 在最后
  const out =
    '[90m4:34AM[0m [33mWRN[0m > configuration issue (/Users/veypi/.aic/config.yaml, ): cannot read configuration file\n' +
    '[90m4:34AM[0m [33mWRN[0m > device tools disabled: invalid authorization configuration\n' +
    '{\n  "version": "v0.10.2",\n  "host": "http://192.168.31.3:4002",\n  "key": ""\n}'
  assert.equal(hostFromConfigOutput(out), 'http://192.168.31.3:4002')
})

test('config get 输出异常时取不到 host（不猜、不抛）', () => {
  assert.equal(hostFromConfigOutput(''), '')
  assert.equal(hostFromConfigOutput('not json at all'), '')
  assert.equal(hostFromConfigOutput('{"host": "https://ivec-ai.com"}'), 'https://ivec-ai.com')
})
