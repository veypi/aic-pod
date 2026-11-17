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

test('/assets/* 从桌面资源目录取（平台图标），穿越仍被拒', async () => {
  const uiDir = fs.mkdtempSync(path.join(os.tmpdir(), 'aic-settings-ui-'))
  const assetsDir = fs.mkdtempSync(path.join(os.tmpdir(), 'aic-settings-assets-'))
  fs.writeFileSync(path.join(uiDir, 'settings.html'), '<h1>settings</h1>')
  fs.writeFileSync(path.join(assetsDir, 'icon.png'), 'PNG')
  let handler
  const protocol = { handle: (_scheme, h) => { handler = h } }
  const session = { fromPartition: () => ({ protocol }) }
  registerHandler({ protocol, session, uiDir, assetsDir })

  const icon = await handler({ url: 'app://aic/assets/icon.png' })
  assert.equal(icon.status, 200)
  assert.match(icon.headers.get('Content-Type'), /image\/png/)
  // 目录穿越：normalize 绝对路径后仍在资源目录内，不会读到目录外的文件。
  const traversal = await handler({ url: 'app://aic/assets/%2e%2e/%2e%2e/package.json' })
  assert.equal(traversal.status, 404)
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
