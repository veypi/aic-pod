const test = require('node:test')
const assert = require('node:assert')
const { createHostPolicy, computeAllowedHosts } = require('./platform-hosts.cjs')

const DEFAULT = 'https://ivec-ai.com'
const LEGACY = ['ivec.ai']

function frame(url) {
  return { senderFrame: { url } }
}

test('默认白名单含配置 host、默认 host 与旧域名', () => {
  assert.deepStrictEqual(computeAllowedHosts('http://localhost:4000', DEFAULT, LEGACY), ['localhost:4000', 'ivec-ai.com', 'ivec.ai'])
})

test('replace 同步配置文件 host，remember 增量并入设置窗确认的新地址', () => {
  const policy = createHostPolicy({ platform: DEFAULT, defaultHost: DEFAULT, legacyHosts: LEGACY })
  policy.replace('http://127.0.0.1:4000')
  assert.deepStrictEqual(policy.all(), ['127.0.0.1:4000', 'ivec-ai.com', 'ivec.ai'])
  policy.remember('https://new.example.com/a')
  assert.ok(policy.all().includes('new.example.com'))
})

test('平台 frame 只认白名单 host，设置 frame 只认 app://aic', () => {
  const policy = createHostPolicy({ platform: DEFAULT, defaultHost: DEFAULT, legacyHosts: LEGACY })
  assert.equal(policy.isPlatformFrame(frame('https://ivec-ai.com/a')), true)
  assert.equal(policy.isPlatformFrame(frame('https://evil.example/a')), false)
  assert.equal(policy.isPlatformFrame({}), false)
  assert.equal(policy.isSettingsFrame(frame('app://aic/settings.html')), true)
  assert.equal(policy.isSettingsFrame(frame('https://ivec-ai.com/settings.html')), false)
})
