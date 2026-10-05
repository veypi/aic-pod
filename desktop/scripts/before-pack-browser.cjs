const path = require('node:path')
const {execFileSync} = require('node:child_process')
const { Arch } = require('builder-util')

module.exports = async (context) => {
  if (context.electronPlatformName !== process.platform)
    throw new Error('Desktop bundles must be built on the target OS')
  execFileSync(process.execPath, [path.join(context.packager.projectDir, 'scripts/sync-cua.mjs')], {stdio:'inherit'})
  const { syncBrowser } = await import('./sync-browser.mjs')
  await syncBrowser({ platform: context.electronPlatformName, arch: Arch[context.arch],
    root: path.join(context.packager.projectDir, 'vendor', 'browser') })
  const { syncAgentBrowser } = await import('./sync-agent-browser.mjs')
  await syncAgentBrowser({ platform: context.electronPlatformName, arch: Arch[context.arch],
    root: path.join(context.packager.projectDir, 'vendor', 'agent-browser') })
}
