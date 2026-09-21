// Desktop supplies only a default executable. Go owns discovery, launch and CDP.
const fs = require('node:fs')
const path = require('node:path')
function browserEnv({ packaged, resourcesPath, directory, platform = process.platform, arch = process.arch, env = process.env }) {
  if (env.AIC_BROWSER_DEFAULT_PATH) return { AIC_BROWSER_DEFAULT_PATH: env.AIC_BROWSER_DEFAULT_PATH }
  const root = packaged ? path.join(resourcesPath, 'browser') : path.join(directory, 'vendor', 'browser')
  const platformRoot = path.join(root, `${platform}-${arch}`)
  const bundled = platform === 'darwin'
    ? path.join(platformRoot, 'Google Chrome for Testing.app', 'Contents', 'MacOS', 'Google Chrome for Testing')
    : path.join(platformRoot, platform === 'win32' ? 'chrome.exe' : 'chrome')
  const candidates = [bundled]
  if (platform === 'darwin') candidates.push('/Applications/Google Chrome.app/Contents/MacOS/Google Chrome')
  if (platform === 'win32') for (const dir of [env.PROGRAMFILES, env['PROGRAMFILES(X86)'], env.LOCALAPPDATA]) {
    if (dir) candidates.push(path.join(dir, 'Google', 'Chrome', 'Application', 'chrome.exe'))
  }
  for (const candidate of candidates) {
    try { if (fs.statSync(candidate).isFile()) return { AIC_BROWSER_DEFAULT_PATH: candidate } } catch {}
  }
  return {}
}
module.exports = { browserEnv }
