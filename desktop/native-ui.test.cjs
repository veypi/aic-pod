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
