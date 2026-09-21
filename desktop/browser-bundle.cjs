const fs = require('node:fs')
const path = require('node:path')
const manifest = require('./browser.json')

function browserExecutable(root, platform, arch) {
  const name = platform === 'darwin'
    ? 'Google Chrome for Testing.app/Contents/MacOS/Google Chrome for Testing'
    : platform === 'win32' ? 'chrome.exe' : 'chrome'
  return path.join(root, `${platform}-${arch}`, name)
}

function assertBrowserBundle(root, platform, arch, config = manifest) {
  const key = `${platform}-${arch}`, asset = config.assets[key]
  if (!asset) throw new Error(`Unsupported bundled Chrome target: ${key}`)
  const dir = path.join(root, key)
  const stamp = JSON.parse(fs.readFileSync(path.join(dir, '.aic-browser.json'), 'utf8'))
  if (stamp.version !== config.version || stamp.sha256 !== asset.sha256 || stamp.target !== key)
    throw new Error(`Bundled Chrome version/checksum mismatch for ${key}; run browser-sync`)
  const executable = browserExecutable(root, platform, arch)
  const required = [executable, path.join(dir, 'ABOUT')]
  if (platform === 'darwin') {
    const contents = path.join(dir, 'Google Chrome for Testing.app', 'Contents')
    required.push(path.join(contents, 'Info.plist'),
      path.join(contents, 'Frameworks', 'Google Chrome for Testing Framework.framework', 'Versions', config.version, 'Google Chrome for Testing Framework'))
  } else {
    required.push(...['icudtl.dat', 'resources.pak', 'locales/en-US.pak', platform === 'win32' ? 'chrome.dll' : 'libEGL.so'].map(name => path.join(dir, name)))
  }
  for (const file of required) {
    if (!fs.statSync(file).isFile()) throw new Error(`Bundled Chrome resource missing: ${file}`)
  }
  return executable
}

module.exports = { browserExecutable, assertBrowserBundle, manifest }
