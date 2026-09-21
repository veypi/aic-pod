const test = require('node:test')
const assert = require('node:assert/strict')
const fs = require('node:fs')
const os = require('node:os')
const path = require('node:path')
const { browserEnv } = require('./browser-path.cjs')
test('desktop injects an independent browser path and preserves an explicit default', () => {
 const root = fs.mkdtempSync(path.join(os.tmpdir(), 'aic-browser-path-'))
 try {
  const bin = path.join(root, 'browser', 'linux-x64', 'chrome')
  fs.mkdirSync(path.dirname(bin), { recursive: true }); fs.writeFileSync(bin, 'fixture')
  const options = { packaged: true, resourcesPath: root, platform: 'linux', arch: 'x64', env: {} }
  assert.deepEqual(browserEnv(options), { AIC_BROWSER_DEFAULT_PATH: bin })
  assert.deepEqual(browserEnv({ ...options, env: { AIC_BROWSER_DEFAULT_PATH: '/custom/chrome' } }), { AIC_BROWSER_DEFAULT_PATH: '/custom/chrome' })
  fs.unlinkSync(bin); assert.deepEqual(browserEnv(options), {})
 } finally { fs.rmSync(root, { recursive: true, force: true }) }
})
