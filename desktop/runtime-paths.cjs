// 随包运行时的可执行文件路径唯一事实源：Chrome for Testing（browser 技能包的
// AIC_BROWSER_PATH 指向它）与官方 agent-browser 原生二进制。
//
// 打包前同步/校验（scripts/sync-browser.mjs、scripts/sync-agent-browser.mjs）与
// 运行时注入（main.js）共用本模块，避免平台文件名表在两处漂移。
const path = require('node:path')

// Chrome for Testing 的可执行文件相对 <root>/<platform>-<arch>/ 的路径。
function chromeExecutableName(platform) {
  return platform === 'darwin'
    ? 'Google Chrome for Testing.app/Contents/MacOS/Google Chrome for Testing'
    : platform === 'win32' ? 'chrome.exe' : 'chrome'
}

// Chrome for Testing 可执行文件的绝对路径（root = vendor/browser 或 resources/browser）。
function browserExecutable(root, platform, arch) {
  return path.join(root, `${platform}-${arch}`, chromeExecutableName(platform))
}

// 官方 agent-browser 原生二进制名。
function agentBrowserBinaryName(platform) {
  return platform === 'win32' ? 'agent-browser.exe' : 'agent-browser'
}

module.exports = { chromeExecutableName, browserExecutable, agentBrowserBinaryName }
