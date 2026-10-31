// 运行时可执行文件路径的契约测试：平台文件名表（runtime-paths.cjs）与注入面
// （main.js）必须同源——browser 技能包只认文件型 AIC_BROWSER_PATH，目录型提示无效。
const test = require('node:test')
const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const { chromeExecutableName, browserExecutable, agentBrowserBinaryName } = require('./runtime-paths.cjs')

test('chrome executable name is per-platform', () => {
  assert.equal(chromeExecutableName('darwin'),
    'Google Chrome for Testing.app/Contents/MacOS/Google Chrome for Testing')
  assert.equal(chromeExecutableName('win32'), 'chrome.exe')
  assert.equal(chromeExecutableName('linux'), 'chrome')
})

test('browser executable lives under <root>/<platform>-<arch>', () => {
  assert.equal(browserExecutable('/res/browser', 'darwin', 'arm64'), path.join('/res/browser',
    'darwin-arm64', 'Google Chrome for Testing.app/Contents/MacOS/Google Chrome for Testing'))
  assert.equal(browserExecutable('/res/browser', 'linux', 'x64'),
    path.join('/res/browser', 'linux-x64', 'chrome'))
  assert.equal(browserExecutable('/res/browser', 'win32', 'x64'),
    path.join('/res/browser', 'win32-x64', 'chrome.exe'))
})

test('agent-browser binary name is per-platform', () => {
  assert.equal(agentBrowserBinaryName('win32'), 'agent-browser.exe')
  assert.equal(agentBrowserBinaryName('linux'), 'agent-browser')
})

test('desktop injects the chrome executable path, not a bundle directory', () => {
  const source = fs.readFileSync(path.join(__dirname, 'main.js'), 'utf8')
  assert.match(source, /AIC_BROWSER_PATH/)
  assert.doesNotMatch(source, /AIC_BROWSER_BUNDLE_DIR/)
})
