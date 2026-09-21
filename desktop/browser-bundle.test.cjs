const test = require('node:test')
const assert = require('node:assert/strict')
const fs = require('node:fs')
const os = require('node:os')
const path = require('node:path')
const { pathToFileURL } = require('node:url')
const { browserExecutable, assertBrowserBundle, manifest } = require('./browser-bundle.cjs')

test('bundle verification rejects missing resources, wrong versions and wrong targets', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'aic-browser-bundle-'))
  try {
    const key = 'linux-x64', dir = path.join(root, key), asset = manifest.assets[key]
    for (const name of ['chrome', 'ABOUT', 'icudtl.dat', 'resources.pak', 'locales/en-US.pak', 'libEGL.so']) {
      const file = path.join(dir, name)
      fs.mkdirSync(path.dirname(file), { recursive: true })
      fs.writeFileSync(file, 'fixture')
    }
    const stamp = { version: manifest.version, target: key, sha256: asset.sha256 }
    const writeStamp = value => fs.writeFileSync(path.join(dir, '.aic-browser.json'), JSON.stringify(value))
    writeStamp(stamp)
    assert.equal(assertBrowserBundle(root, 'linux', 'x64'), browserExecutable(root, 'linux', 'x64'))
    writeStamp({ ...stamp, version: 'old' })
    assert.throws(() => assertBrowserBundle(root, 'linux', 'x64'), /mismatch/)
    writeStamp({ ...stamp, target: 'win32-x64' })
    assert.throws(() => assertBrowserBundle(root, 'linux', 'x64'), /mismatch/)
    writeStamp(stamp)
    fs.unlinkSync(path.join(dir, 'icudtl.dat'))
    assert.throws(() => assertBrowserBundle(root, 'linux', 'x64'), /icudtl/)
  } finally { fs.rmSync(root, { recursive: true, force: true }) }
})

test('an archive checksum failure preserves the previous browser payload', async () => {
  const { syncBrowser } = await import(pathToFileURL(path.join(__dirname, 'scripts/sync-browser.mjs')).href)
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'aic-browser-sync-'))
  try {
    const dest = path.join(root, 'linux-x64')
    fs.mkdirSync(dest)
    fs.writeFileSync(path.join(dest, 'keep'), 'previous')
    const archive = path.join(root, 'wrong.zip')
    fs.writeFileSync(archive, 'not the pinned archive')
    await assert.rejects(syncBrowser({ root, archive, platform: 'linux', arch: 'x64' }), /SHA-256 mismatch/)
    assert.equal(fs.readFileSync(path.join(dest, 'keep'), 'utf8'), 'previous')
    assert.deepEqual(fs.readdirSync(root).sort(), ['linux-x64', 'wrong.zip'])
  } finally { fs.rmSync(root, { recursive: true, force: true }) }
})
