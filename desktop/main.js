// AIC Desktop — Electron 主进程（纯远程壳：Chromium 渲染平台 + Go 后端子进程）。
//
// 架构（2026-08-07 远程化改造，替代 iframe 壳页面方案）：
//
//	Electron Main (Node)
//	 ├─ 启动：主窗口先加载本地 loading.html → spawn Go 后端（AIC_PORT_FILE 握手）
//	 │    → 读配置 host → 探测 {host}/root.html → 跳转平台页 or 本地 /settings
//	 ├─ 平台页（{host}/ 顶层页面）：session.setPreloads 注入 remote-preload.js
//	 │    （host 白名单过滤后暴露 window.aicDesktop：api 转发/窗口控制/外链/桌宠）
//	 ├─ 本地设置窗口：独立 partition + settings-preload（仅探测/跳转两个能力）
//	 └─ 托盘：打开 / 本地配置 / 退出；桌宠 = 透明小窗加载 {host}/pet
//
// 安全：所有 IPC handler 校验 event.senderFrame.url 的 host——
// 平台能力（local:api/window:*/pet:*）仅白名单 host（配置 host + ivec.ai）可调；
// 设置能力（platform:check/open）仅 127.0.0.1 本地页面可调。端口/code 不出主进程。
const { app, BrowserWindow, Tray, Menu, ipcMain, shell, dialog, session, screen } = require('electron')
const { spawn } = require('child_process')
const fs = require('fs')
const net = require('net')
const path = require('path')

// ---- 常量 ----
const isDev = !app.isPackaged
// kiosk/展示场景：AIC_FULLSCREEN=1 或 --fullscreen 参数 → 启动即全屏（大屏展示用）
const startFullscreen =
  process.env.AIC_FULLSCREEN === '1' || process.argv.includes('--fullscreen')
const backendBin = isDev
  ? path.join(__dirname, 'bin', 'aic-backend' + (process.platform === 'win32' ? '.exe' : ''))
  : path.join(process.resourcesPath, 'backend', 'aic-backend' + (process.platform === 'win32' ? '.exe' : ''))
const trayIcon = process.platform === 'darwin'
  ? path.join(__dirname, 'assets', 'trayTemplate.png')
  : path.join(__dirname, 'assets', 'tray.png')
let petSize = 100 // 桌宠窗口边长（右键菜单缩放 50–400，随 pet-pos.json 持久化）
const probeTimeout = 5000 // {host}/root.html 探测超时
const DEFAULT_HOST = 'https://ivec.ai'

let mainWin = null // 主窗口（loading → 平台页 / 本地设置页）
let petWin = null // 桌宠窗口（透明小窗，与主窗口共存，加载 /pet 或 /a/{aid}/pet）
let settingsWin = null // 本地设置窗口（系统边框，独立 partition）
let tray = null
let backend = null
let quitting = false
let petDragOff = null // 桌宠拖动：鼠标相对窗口偏移
let petPos = null // 桌宠当前位置 {x, y}（内存缓存，创建/拖动时更新）
let petPosTimer = null // 桌宠位置写盘防抖 timer
let localPort = 0 // Go 后端本地服务端口
let localCode = '' // 本地 API 校验码（不出主进程）
let host = DEFAULT_HOST // 平台地址（配置读取）

// ---- 单实例（唯一 ID，第二实例聚焦现有窗口；本地服务端口唯一） ----
if (!app.requestSingleInstanceLock()) {
  app.quit()
} else {
  app.on('second-instance', () => focusMain())
  app.whenReady().then(start)
  app.on('activate', () => focusMain()) // mac Dock 图标点击
  app.on('before-quit', () => { quitting = true })
}

// ---- 应用菜单 ----
// mac 保留应用/编辑/视图菜单（Cmd+Q / Cmd+C+V / 开发者工具）；win/linux frameless 无菜单栏。
if (process.platform === 'darwin') {
  Menu.setApplicationMenu(Menu.buildFromTemplate([
    { role: 'appMenu' },
    { role: 'editMenu' },
    {
      label: '视图',
      submenu: [
        { role: 'reload', label: '重新加载' },
        { role: 'toggleDevTools', label: '开发者工具' },
        { type: 'separator' },
        { role: 'togglefullscreen', label: '切换全屏' },
      ],
    },
    { role: 'windowMenu' },
  ]))
} else {
  Menu.setApplicationMenu(null)
}

async function start() {
  // 1. 主窗口先加载本地 loading（静态文件，无需后端）
  createMainWindow(() => {
    mainWin.loadFile(path.join(__dirname, 'loading.html'))
  })
  // 平台页注入：session 级 preload（所有 frame 生效，host 白名单过滤）
  session.defaultSession.setPreloads([path.join(__dirname, 'remote-preload.js')])

  registerIpc()
  startCmdServer()

  // 2. 异步链：spawn 后端 → 握手 → 读配置 → 探测平台 → 跳转
  setStep('正在启动本地服务…')
  const info = await spawnBackend()
  if (!info) {
    dialog.showErrorBox('AIC Desktop', '后端启动超时')
    app.quit()
    return
  }
  localPort = info.port
  localCode = info.code

  setStep('正在读取配置…')
  const cfg = await getLocalConfig()
  if (cfg && cfg.host) host = cfg.host
  allowedHostsCache = computeAllowedHosts(host)
  // 默认打开地址：host + home_path（默认 /；非法值回退 /）
  let homePath = (cfg && cfg.home_path) || '/'
  if (typeof homePath !== 'string' || !homePath.startsWith('/') || homePath.startsWith('//')) homePath = '/'

  setStep('正在检测平台 ' + host + ' …')
  const reachable = await probeRoot(host)

  // 3. 跳转：平台可达 → host + homePath；否则本地 /settings 配置
  if (reachable) {
    loadMain(host.replace(/\/+$/, '') + homePath)
  } else {
    loadMainLocal('/settings')
    openSettings() // 配置窗口提示用户填 host
  }

  try {
    createTray()
  } catch (err) {
    console.error('[tray] create failed:', err.message)
  }

  // 应用退出时停止后端（SIGTERM → Go 优雅退出）
  app.on('will-quit', () => {
    if (backend && !backend.killed) backend.kill('SIGTERM')
  })
}

// ---- 启动子进程与握手 ----
function spawnBackend() {
  return new Promise((resolve) => {
    const portFile = path.join(app.getPath('userData'), 'aic-port.json')
    try { fs.rmSync(portFile, { force: true }) } catch (e) { /* 忽略 */ }
    backend = spawn(backendBin, [], {
      env: { ...process.env, AIC_PORT_FILE: portFile, AIC_DEVICE_TYPE: 'desktop' },
      stdio: ['ignore', 'pipe', 'pipe'],
    })
    backend.stdout.on('data', (d) => console.log('[backend]', d.toString().trim()))
    backend.stderr.on('data', (d) => console.log('[backend]', d.toString().trim()))
    backend.on('exit', (code) => {
      if (!quitting) {
        dialog.showErrorBox('AIC Desktop', `后端进程异常退出 (${code})`)
        app.quit()
      }
    })
    // 轮询端口文件（最多 15s）
    const deadline = Date.now() + 15000
    const tryRead = () => {
      try {
        const info = JSON.parse(fs.readFileSync(portFile, 'utf8'))
        if (info && info.port && info.code) return resolve(info)
      } catch (e) { /* 未写入/未完整 */ }
      if (Date.now() > deadline) return resolve(null)
      setTimeout(tryRead, 100)
    }
    tryRead()
  })
}

// ---- 主进程内部 HTTP（本地 API / 平台探测） ----
async function getLocalConfig() {
  try {
    const r = await fetch(`http://127.0.0.1:${localPort}/api/get_config`, {
      headers: { 'x-aic-code': localCode },
      signal: AbortSignal.timeout(3000),
    })
    return r.ok ? await r.json() : null
  } catch (e) {
    return null
  }
}

async function probeRoot(url) {
  const base = String(url || '').replace(/\/+$/, '')
  if (!/^https?:\/\//.test(base)) return false
  try {
    const r = await fetch(base + '/root.html', {
      signal: AbortSignal.timeout(probeTimeout),
    })
    return r.ok || r.status === 304
  } catch (e) {
    return false
  }
}

// ---- 白名单（注入与 IPC 校验共用） ----
let allowedHostsCache = [DEFAULT_HOST.replace(/^https?:\/\//, '')]
function computeAllowedHosts(platform) {
  const list = []
  const add = (u) => {
    try {
      const h = new URL(u).host
      if (h && !list.includes(h)) list.push(h)
    } catch (e) { /* 忽略 */ }
  }
  add(platform)
  add(DEFAULT_HOST)
  return list
}

// 校验 IPC 调用方 frame 是否平台白名单（host ∈ 配置 host / ivec.ai）
function isPlatformFrame(event) {
  try {
    const h = new URL(event.senderFrame.url).host
    return allowedHostsCache.includes(h)
  } catch (e) {
    return false
  }
}

// 校验调用方是否本地设置窗口（127.0.0.1:port）
function isLocalFrame(event) {
  try {
    const u = new URL(event.senderFrame.url)
    return u.hostname === '127.0.0.1' || u.hostname === 'localhost'
  } catch (e) {
    return false
  }
}

// ---- 本地指令通道（aic wake 子指令 → pet 组件事件）----
// unix socket：{appData}/aic/desktop.sock（0600，仅本机同用户进程可连；
// 路径与 Go 端 os.UserConfigDir()/aic 同位置）。协议 = 换行分隔 JSON 请求/应答。
// windows 下 Node 走命名管道、与 Go 端拨号不兼容，暂不开启。
let cmdServer = null

function cmdSockPath() {
  return path.join(app.getPath('appData'), 'aic', 'desktop.sock')
}

function startCmdServer() {
  if (process.platform === 'win32') return
  const p = cmdSockPath()
  try { fs.mkdirSync(path.dirname(p), { recursive: true }) } catch (_) { /* 忽略 */ }
  try { fs.unlinkSync(p) } catch (_) { /* 忽略 */ }
  cmdServer = net.createServer((conn) => {
    let buf = ''
    conn.on('data', (d) => {
      buf += d
      let i
      while ((i = buf.indexOf('\n')) >= 0) {
        const line = buf.slice(0, i).trim()
        buf = buf.slice(i + 1)
        if (line) handleCmd(line, conn)
      }
    })
    conn.on('error', () => { })
  })
  cmdServer.on('error', (e) => console.error('[cmd] server:', e.message))
  cmdServer.listen(p, () => { try { fs.chmodSync(p, 0o600) } catch (_) { /* 忽略 */ } })
  app.on('will-quit', () => {
    try { cmdServer && cmdServer.close() } catch (_) { /* 忽略 */ }
    try { fs.unlinkSync(p) } catch (_) { /* 忽略 */ }
  })
}

// 指令分发：wake = 唤醒 pet 录音（与 pet 页左键单击同效）。转发桌宠窗与主窗口
// （非 pet 页无监听器自动丢弃）；无任何窗口存活时返回错误供 CLI 退出码反馈
function handleCmd(line, conn) {
  const reply = (o) => { try { conn.end(JSON.stringify(o) + '\n') } catch (_) { /* 忽略 */ } }
  let cmd = null
  try { cmd = JSON.parse(line) } catch (_) { return reply({ ok: false, error: 'invalid json' }) }
  if (!cmd || cmd.action !== 'wake') return reply({ ok: false, error: 'unknown action' })
  let delivered = false
  for (const w of [petWin, mainWin]) {
    if (w && !w.isDestroyed()) { w.webContents.send('pet:cmd', { action: 'wake' }); delivered = true }
  }
  reply(delivered ? { ok: true } : { ok: false, error: 'no window alive' })
}

// ---- IPC ----
function registerIpc() {
  // 白名单下发（remote-preload 顶层 sendSync）
  ipcMain.on('allowed:hosts', (e) => { e.returnValue = allowedHostsCache })

  // 本地 API 转发（平台页 → 本地服务，code 由主进程持有）
  ipcMain.handle('local:api', async (event, name, args) => {
    if (!isPlatformFrame(event)) throw new Error('forbidden')
    const m = String(name || '')
    if (!/^[a-z_]+$/.test(m)) throw new Error('invalid method')
    const isGet = ['ping', 'get_config', 'get_status', 'get_log'].includes(m)
    try {
      const init = { headers: { 'x-aic-code': localCode }, signal: AbortSignal.timeout(15000) }
      if (!isGet) {
        init.method = 'POST'
        init.headers['Content-Type'] = 'application/json'
        init.body = JSON.stringify(args || {})
      }
      const r = await fetch(`http://127.0.0.1:${localPort}/api/${m}`, init)
      const d = await r.json().catch(() => ({}))
      if (!r.ok) throw new Error(d.message || `HTTP ${r.status}`)
      return d
    } catch (e) {
      throw new Error(e.message || String(e))
    }
  })

  // 窗口控制（平台页桌面版 header / 桌宠页）
  ipcMain.handle('window:minimise', (e) => { if (isPlatformFrame(e)) mainWin?.minimize(); return state() })
  ipcMain.handle('window:maximise', (e) => {
    if (isPlatformFrame(e) && mainWin) mainWin.isMaximized() ? mainWin.unmaximize() : mainWin.maximize()
    return state()
  })
  ipcMain.handle('window:close', (e) => { if (isPlatformFrame(e)) mainWin?.hide(); return state() })
  ipcMain.handle('window:fullscreen', (e) => {
    if (isPlatformFrame(e) && mainWin) mainWin.setFullScreen(!mainWin.isFullScreen())
    return state()
  })
  ipcMain.handle('window:pet', (e, page) => (isPlatformFrame(e) ? enterPet(page) : state()))
  ipcMain.handle('window:restore', (e) => (isPlatformFrame(e) ? leavePet() : state()))

  // 外链 → 系统默认浏览器（平台页拦截普通外链）
  ipcMain.handle('open:external', (e, url) => {
    if (!isPlatformFrame(e)) return false
    const u = String(url || '')
    if (!/^https?:\/\//.test(u)) return false
    shell.openExternal(u)
    return true
  })

  // 桌宠右键菜单：打开/隐藏对话框（有 agent 时，由渲染进程携带状态）/ 缩小 / 放大 / 关闭
  ipcMain.on('pet:menu', (e, opts) => {
    if (!petWin || e.sender !== petWin.webContents) return
    const o = opts || {}
    const items = []
    if (o.hasAgent) {
      items.push({ label: o.dialogVisible ? '隐藏对话框' : '打开对话框', click: () => petWin?.webContents.send('pet:toggle-dialog') })
      items.push({ type: 'separator' })
    }
    items.push(
      { label: '缩小', click: () => resizePet(-1) },
      { label: '放大', click: () => resizePet(1) },
      { type: 'separator' },
      { label: '关闭', click: () => leavePet() },
    )
    Menu.buildFromTemplate(items).popup({ window: petWin })
  })

  // 桌宠拖动（pet 页）
  ipcMain.on('pet:drag-start', (e, { x, y }) => {
    if (!isPlatformFrame(e) || !petWin) return
    const [wx, wy] = petWin.getPosition()
    petDragOff = [x - wx, y - wy]
  })
  ipcMain.on('pet:drag-move', (e, { x, y }) => {
    if (!isPlatformFrame(e) || !petWin || !petDragOff) return
    petWin.setPosition(Math.round(x - petDragOff[0]), Math.round(y - petDragOff[1]))
    petPos = petWin.getPosition()
    savePetPosDebounced()
  })

  // 设置窗口能力（仅 127.0.0.1 本地页面）
  ipcMain.handle('platform:check', async (e, url) => {
    if (!isLocalFrame(e)) return { ok: false, error: 'forbidden' }
    const ok = await probeRoot(url)
    return { ok, url: String(url || '').replace(/\/+$/, '') + '/root.html' }
  })
  ipcMain.handle('platform:open', (e, url) => {
    if (!isLocalFrame(e)) return false
    const u = String(url || '')
    if (!/^https?:\/\//.test(u)) return false
    loadMain(u)
    closeSettings()
    return true
  })
}

// ---- 窗口 ----
function createMainWindow(init) {
  mainWin = new BrowserWindow({
    width: 1280,
    height: 800,
    minWidth: 800,
    minHeight: 600,
    frame: false,
    show: false,
    fullscreen: startFullscreen,
    backgroundColor: '#ffffff',
    webPreferences: {
      contextIsolation: true,
      nodeIntegration: false,
      sandbox: true,
    },
  })
  mainWin.once('ready-to-show', () => {
    if (startFullscreen) mainWin.setFullScreen(true)
    mainWin.show()
  })
  mainWin.on('close', (e) => {
    if (!quitting) {
      e.preventDefault()
      mainWin.hide()
    }
  })
  // 平台页 target=_blank → 系统浏览器
  mainWin.webContents.setWindowOpenHandler(({ url }) => {
    if (/^https?:\/\//.test(url)) shell.openExternal(url)
    return { action: 'deny' }
  })
  if (init) init()
}

function loadMain(url) {
  if (!mainWin) return
  mainWin.loadURL(url)
  mainWin.show()
  mainWin.focus()
}

function loadMainLocal(pathname) {
  loadMain(`http://127.0.0.1:${localPort}/${pathname}?code=${encodeURIComponent(localCode)}`)
}

function setStep(text) {
  mainWin?.webContents.executeJavaScript(`window.__setStep && window.__setStep(${JSON.stringify(text)})`).catch(() => { })
}

// ---- 本地设置窗口（系统边框，独立 partition：登录态/存储与平台隔离） ----
function openSettings() {
  if (settingsWin && !settingsWin.isDestroyed()) {
    settingsWin.show()
    settingsWin.focus()
    return
  }
  settingsWin = new BrowserWindow({
    width: 760,
    height: 640,
    frame: true,
    parent: mainWin,
    webPreferences: {
      partition: 'settings',
      preload: path.join(__dirname, 'settings-preload.js'),
      contextIsolation: true,
      nodeIntegration: false,
      sandbox: true,
    },
  })
  settingsWin.loadURL(`http://127.0.0.1:${localPort}/settings?code=${encodeURIComponent(localCode)}`)
  settingsWin.on('closed', () => { settingsWin = null })
}

function closeSettings() {
  if (settingsWin && !settingsWin.isDestroyed()) settingsWin.close()
}

// ---- 托盘：win 右下角 / mac 菜单栏 ----
function createTray() {
  tray = new Tray(trayIcon)
  tray.setToolTip('AIC Desktop')
  tray.setContextMenu(Menu.buildFromTemplate([
    { label: '打开', click: () => focusMain() },
    { label: '本地配置', click: () => openSettings() },
    { type: 'separator' },
    { label: '退出', click: () => { quitting = true; app.quit() } },
  ]))
  tray.on('click', () => focusMain())
}

// ---- 桌宠位置/尺寸缓存：userData/pet-pos.json {x,y,size}（进入恢复上次，拖动/缩放防抖落盘） ----
function petPosFile() {
  return path.join(app.getPath('userData'), 'pet-pos.json')
}

function loadPetState() {
  try {
    const p = JSON.parse(fs.readFileSync(petPosFile(), 'utf-8'))
    if (p && typeof p === 'object') return p
  } catch (_) { /* 首次/损坏 → 无缓存 */ }
  return {}
}

function savePetPosNow() {
  clearTimeout(petPosTimer)
  petPosTimer = null
  if (!petPos) return
  try { fs.writeFileSync(petPosFile(), JSON.stringify({ ...petPos, size: petSize })) } catch (_) { /* 忽略 */ }
}

function savePetPosDebounced() {
  clearTimeout(petPosTimer)
  petPosTimer = setTimeout(savePetPosNow, 300)
}

// 缓存位置是否落在任一显示器工作区内（跨会话显示器可能变化，失效则回退鼠标位置）
function petPosOnScreen(x, y) {
  return screen.getAllDisplays().some((d) => {
    const b = d.workArea
    return x >= b.x && x < b.x + b.width && y >= b.y && y < b.y + b.height
  })
}

// 缩放桌宠：×1.25 / ×0.8 步进，50–400，以窗口中心为锚点
function resizePet(dir) {
  if (!petWin) return
  const next = Math.min(400, Math.max(50, Math.round(petSize * (dir > 0 ? 1.25 : 0.8))))
  if (next === petSize) return
  const [x, y] = petWin.getPosition()
  const d = next - petSize
  petSize = next
  petWin.setSize(petSize, petSize)
  petWin.setPosition(Math.round(x - d / 2), Math.round(y - d / 2))
  petPos = petWin.getPosition()
  savePetPosDebounced()
}

// ---- 桌宠：独立透明小窗，与主窗口共存（主窗口不动），加载 /pet 或 /a/{aid}/pet ----
function enterPet(page) {
  if (!mainWin) return state()
  if (petWin) return state()
  // 恢复上次尺寸（默认 100）
  const st = loadPetState()
  petSize = Number.isFinite(st.size) ? Math.min(400, Math.max(50, Math.round(st.size))) : 100
  petWin = new BrowserWindow({
    width: petSize,
    height: petSize,
    frame: false,
    transparent: true,
    resizable: false,
    alwaysOnTop: true,
    skipTaskbar: true,
    hasShadow: false,
    webPreferences: {
      contextIsolation: true,
      nodeIntegration: false,
      sandbox: true,
    },
  })
  // 平台桌宠页（仅放行 /pet 与 /a/{aid}/pet，query 透传）；remote-preload 注入（host 白名单）
  const raw = String(page || '')
  const p = /^\/pet($|\?)|^\/a\/[^/]+\/pet($|\?)/.test(raw) ? raw : '/pet'
  petWin.loadURL(host.replace(/\/+$/, '') + p)
  // 位置：优先恢复上次缓存（仍在显示器内），无缓存/失效居中于鼠标
  if (Number.isFinite(st.x) && Number.isFinite(st.y) && petPosOnScreen(st.x, st.y)) {
    petWin.setPosition(st.x, st.y)
  } else {
    const c = screen.getCursorScreenPoint()
    petWin.setPosition(Math.round(c.x - petSize / 2), Math.round(c.y - petSize / 2))
  }
  petPos = petWin.getPosition()
  return state()
}

// 双击桌宠：仅销毁小窗，主窗口不动
function leavePet() {
  if (petWin) { petWin.destroy(); petWin = null }
  petDragOff = null
  savePetPosNow() // 防抖未落盘时兜底写盘
  return state()
}

function state() {
  return {
    desktop: true,
    maximised: mainWin ? mainWin.isMaximized() : false,
    fullscreen: mainWin ? mainWin.isFullScreen() : false,
  }
}

function focusMain() {
  if (!mainWin) return
  if (mainWin.isMinimized()) mainWin.restore()
  mainWin.show()
  mainWin.focus()
}
