const path = require('node:path')
const fs = require('node:fs')
const { Arch } = require('builder-util')

module.exports = async (context) => {
  if (context.electronPlatformName !== process.platform)
    throw new Error('Desktop bundles must be built on the target OS')
  // browser.zip 由 skill-packages/browser/build.sh 产出（make browser-zip）；缺失 =
  // 打包链路漏步骤，显式失败（builtin 预装包是桌面形态的浏览器能力来源）。
  const zip = path.join(context.packager.projectDir, '..', 'skill-packages', 'browser', 'browser.zip')
  if (!fs.existsSync(zip))
    throw new Error(`browser.zip missing: ${zip}（先跑 make browser-zip / skill-packages/browser/build.sh）`)
  const { syncBrowser } = await import('./sync-browser.mjs')
  await syncBrowser({ platform: context.electronPlatformName, arch: Arch[context.arch],
    root: path.join(context.packager.projectDir, 'vendor', 'browser') })
}
