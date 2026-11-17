'use strict'

const test = require('node:test')
const assert = require('node:assert')
const fs = require('node:fs')
const vm = require('node:vm')

function read(file) {
  return fs.readFileSync(require('node:path').join(__dirname, file), 'utf8')
}

for (const file of ['settings-ui/login.html', 'settings-ui/settings.html']) {
  test(file + ' 内联脚本可解析且 DOM id 引用存在', () => {
    const html = read(file)
    const ids = new Set([...html.matchAll(/\bid="([^"]+)"/g)].map(match => match[1]))
    for (const match of html.matchAll(/<script[^>]*>([\s\S]*?)<\/script>/g)) {
      new vm.Script(match[1], { filename: file })
    }
    for (const match of html.matchAll(/\$\('([^']+)'\)/g)) {
      assert.ok(ids.has(match[1]), `${file} 缺少 #${match[1]}`)
    }
  })
}

test('原生登录桥与页面不接触任何 token 或 Host protocol secret', () => {
  const source = read('login-preload.js') + read('settings-ui/login.html')
  for (const forbidden of ['access_token', 'refresh_token', 'protocol_secret', 'auth_set_token', 'local:api']) {
    assert.ok(!source.includes(forbidden), 'login surface contains ' + forbidden)
  }
})

test('平台桥分流 aicHost 与固定 Desktop 管理方法，不暴露任意 local:api', () => {
  const source = read('remote-preload.js')
  assert.match(source, /exposeInMainWorld\('aicHost'/)
  assert.match(source, /exposeInMainWorld\('aicDesktop'/)
  assert.ok(!source.includes('api: (method'))
  assert.ok(!source.includes('auth_set_token'))
  assert.match(source, /auth: true/)
  assert.match(source, /files: false/)
})

test('Desktop 设置不再提供手工设备 key 绑定', () => {
  const source = read('settings-ui/settings.html')
  assert.ok(!source.includes('id="key"'))
  assert.ok(!source.includes('$pod.bind'))
  assert.ok(!source.includes('$pod.unbind'))
})

test('桌面主进程与 preload 脚本语法可解析', () => {
  // 页面测试只解析 HTML 内联脚本；main.js 这类主进程入口必须在测试里也编译一遍，
  // 否则一个多余括号就会一直到用户重启 Electron 才暴露（2026-11-17 实际踩过）。
  const files = [
    'main.js', 'native-app.cjs', 'native-auth.cjs', 'auth-cookies.cjs', 'auth-store.cjs',
    'electron-cookies.cjs', 'device-identity.cjs', 'session-rpc.cjs', 'settings-protocol.cjs',
    'platform-hosts.cjs', 'host-state.cjs', 'backend.cjs', 'backend-startup.cjs',
    'login-preload.js', 'settings-preload.js', 'remote-preload.js', 'secure-origin.cjs', 'runtime-paths.cjs',
  ]
  for (const file of files) {
    assert.doesNotThrow(() => new vm.Script(read(file), { filename: file }), file + ' 语法错误')
  }
})

test('原生登录页平铺（不再套第二层卡片）且顶部对齐', () => {
  const source = read('settings-ui/login.html')
  const cardRule = source.match(/\.card \{[^}]*\}/)
  assert.ok(cardRule, '.card 规则缺失')
  assert.ok(!/background:\s*var\(--card\)/.test(cardRule[0]), '登录页仍给卡片加了背景')
  assert.ok(!/box-shadow/.test(cardRule[0]), '登录页仍给卡片加了阴影')
  assert.match(source, /justify-content:flex-start/, '登录页未做顶部对齐（切换登录方式会整页跳动）')
})

test('登出后主窗口让位给登录窗', () => {
  const source = read('main.js')
  const start = source.indexOf('function showNativeLogin()')
  const end = source.indexOf('function openAuthWindow(')
  assert.ok(start > 0 && end > start)
  assert.match(source.slice(start, end), /mainWin\.hide\(\)/)
})

test('原生登录页：不显示平台服务器地址、用平台图标、带三方登录入口', () => {
  const source = read('settings-ui/login.html')
  assert.ok(!source.includes('id="server"'), 'login page still shows the server address')
  assert.ok(!source.includes('login.server'), 'login page still renders the server label')
  assert.match(source, /src="\/assets\/icon\.png"/)
  assert.match(source, /third_party/)
  assert.match(source, /config\.providers/)
})

test('原生登录页开在独立小窗口，不再塞进平台视图', () => {
  const source = read('main.js')
  const start = source.indexOf('function showNativeLogin()')
  const end = source.indexOf('function openAuthWindow(')
  assert.ok(start > 0 && end > start)
  const body = source.slice(start, end)
  assert.ok(!body.includes('platformView.webContents.loadURL'))
  assert.match(body, /loginWin = new BrowserWindow\(/)
})
