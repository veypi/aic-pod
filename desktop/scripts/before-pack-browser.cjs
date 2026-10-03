const path = require('node:path')
const { Arch } = require('builder-util')

module.exports = async (context) => {
  if (context.electronPlatformName !== process.platform)
    throw new Error('Desktop bundles must be built on the target OS')
  const { syncBrowser } = await import('./sync-browser.mjs')
  await syncBrowser({ platform: context.electronPlatformName, arch: Arch[context.arch],
    root: path.join(context.packager.projectDir, 'vendor', 'browser') })
}
