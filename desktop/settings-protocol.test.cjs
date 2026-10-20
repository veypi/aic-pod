const test = require('node:test')
const assert = require('node:assert')
const fs = require('node:fs')
const os = require('node:os')
const path = require('node:path')
const { registerScheme, registerHandler, SETTINGS_SCHEME } = require('./settings-protocol.cjs')

test('registerScheme 声明 standard+secure（localStorage 与真实 origin）', () => {
  let captured
  registerScheme({ registerSchemesAsPrivileged: (v) => { captured = v } })
  assert.equal(captured[0].scheme, SETTINGS_SCHEME)
  assert.equal(captured[0].privileges.standard, true)
  assert.equal(captured[0].privileges.secure, true)
})

test('handler 只服务 uiDir 内文件并回退 settings.html', async () => {
  const uiDir = fs.mkdtempSync(path.join(os.tmpdir(), 'aic-settings-'))
  fs.writeFileSync(path.join(uiDir, 'settings.html'), '<h1>settings</h1>')
  fs.mkdirSync(path.join(uiDir, 'sub'))
  fs.writeFileSync(path.join(uiDir, 'sub', 'x.css'), 'body{}')
  let handler
  const protocol = { handle: (_scheme, h) => { handler = h } }
  const session = { fromPartition: () => ({ protocol }) }
  registerHandler({ protocol, session, uiDir })

  const root = await handler({ url: 'app://aic/' })
  assert.equal(root.status, 200)
  assert.match(await root.text(), /settings/)
  const css = await handler({ url: 'app://aic/sub/x.css' })
  assert.equal(css.headers.get('Content-Type'), 'text/css')
  const missing = await handler({ url: 'app://aic/nope.js' })
  assert.equal(missing.status, 404)
  const traversal = await handler({ url: 'app://aic/../package.json' })
  assert.equal(traversal.status, 404)
})
