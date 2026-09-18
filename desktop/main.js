// AIC Desktop — Electron 主进程（纯远程壳：Chromium 渲染平台 + Go 后端子进程）。
//
// 架构（固定视口 browser，设计见 aic/docs/os_native_windows.md）：
//
// Electron Main (Node)
//   ├─ spawn Go 后端 → 本地 API 配置/绑定 → 平台页 WebContentsView
//   ├─ browser provider（browser-tool.mjs）：固定视口离屏 BrowserWindow 标签池
//   │    默认 1280×720、DPR=1；与主窗口尺寸、可见性和焦点无关
//   ├─ nativeWin 桥：离屏 paint → 平台 canvas 等比例展示 → 帧确认背压
//   │    rect 只用于显示与输入换算；键盘/IME 焦点保留在平台页，输入经 CDP 转发
//	 ├─ 本地配置 = 独立设置窗口（系统边框，settings-preload；不依赖平台页）：托盘
//	 │    「本地配置」直开；平台不可达首配时自动打开（主窗停留 loading 提示）
//	 └─ 托盘：打开 / 本地配置 / 打开配置目录 / 退出；桌宠 = 透明小窗加载 {host}/pet
//
// 安全：所有 IPC handler 校验 event.senderFrame.url 的 host——
// 平台能力（local:api/window:*/pet:*/native:*）仅白名单 host（配置 host + 默认与旧平台域名）可调；
// 设置能力（platform:check/open、settings:close）仅 127.0.0.1 本地页面可调。端口/code 不出主进程。
const { app, BaseWindow, BrowserWindow, WebContentsView, Tray, Menu, ipcMain, shell, dialog, session, screen, globalShortcut } = require('electron')
const { spawn } = require('child_process')
const fs = require('fs')
const net = require('net')
const path = require('path')
const { normMods, leaderHit } = require('./leader-grab')

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
const DEFAULT_HOST = 'https://ivec-ai.com'
// LEGACY_HOSTS：旧平台域名（ivec.ai 现 301 至 ivec-ai.com），作为受信任 host 保留，
// 兼容旧配置与跳转后的页面 origin。
const LEGACY_HOSTS = ['ivec.ai']

let mainWin = null // 主窗口（BaseWindow：平台页与 browser 画面展示）
let platformView = null // 平台页视图（恒占满 contentView）
let aiBrowser = null // browser 壳通道（adapter.tabControl = 原生内容池控制面）
let petWin = null // 桌宠窗口（透明小窗，与主窗口共存，加载 /pet 或 /a/{aid}/pet）
let settingsWin = null // 本地配置独立窗口（BrowserWindow，系统边框，独立 partition + settings-preload）
let keepWin = null // worker 保活窗口（隐藏；与平台页同 session 同源，持 nc SharedWorker 端口）
let keepExpectedUrl = '' // keepWin 当前期望地址（did-fail-load 延迟重试的竞态护栏）
let tray = null
let backend = null
let quitting = false
let petDragOff = null // 桌宠拖动：鼠标相对窗口偏移
let petPos = null // 桌宠当前位置 {x, y}（内存缓存，创建/拖动时更新）
let petPosTimer = null // 桌宠位置写盘防抖 timer
let localPort = 0 // Go 后端本地服务端口
let localCode = '' // 本地 API 校验码（不出主进程）
let host = DEFAULT_HOST // 平台地址（配置读取）
let browserInputFocused = false // 平台 viewer 的键盘输入代理是否聚焦

// ---- 单实例（唯一 ID，第二实例聚焦现有窗口；本地服务端口唯一） ----
if (!app.requestSingleInstanceLock()) {
  app.quit()
} else {
  app.on('second-instance', () => focusMain())
  app.whenReady().then(start)
  app.on('activate', () => focusMain()) // mac Dock 图标点击
  app.on('before-quit', () => { quitting = true })
}

// ---- 后端子进程回收（will-quit → SIGTERM → Go 优雅退出） ----
// 注册在启动流程最前：除正常退出外，spawn 超时/启动中途失败等提前 app.quit() 的
// 路径同样要回收子进程（原先注册在 start() 末尾，这些路径漏 kill 会留下孤儿后端）。
app.on('will-quit', () => {
  if (backend && !backend.killed) backend.kill('SIGTERM')
})

// ---- 应用菜单 ----
// mac 保留应用/编辑/视图菜单（Cmd+Q / Cmd+C+V / 开发者工具）；win/linux frameless 无菜单栏。
// 应用菜单第一项不用 role:'appMenu'——dev 模式（npm start）下其 label 取进程 bundle 名
// 固定显示 "Electron"；显式 label 让菜单栏在 dev/打包两种形态都显示 AIC Desktop。
if (process.platform === 'darwin') {
  Menu.setApplicationMenu(Menu.buildFromTemplate([
    {
      label: 'AIC Desktop',
      submenu: [
        { role: 'about', label: '关于 AIC Desktop' },
        { type: 'separator' },
        { role: 'hide', label: '隐藏 AIC Desktop' },
        { role: 'hideOthers', label: '隐藏其他应用程序' },
        { role: 'unhide', label: '全部显示' },
        { type: 'separator' },
        { role: 'quit', label: '退出 AIC Desktop' },
      ]
    },
    { role: 'editMenu' },
    {
      label: '视图',
      // 主窗口是 BaseWindow（无 webContents），role:reload/toggleDevTools/togglefullscreen
      // 会找 focusedWindow().webContents → undefined 抛异常（2026-09-10 实测报错）——
      // 三个能力全部改成显式 handler。
      submenu: [
        { label: '重新加载', accelerator: 'CmdOrCtrl+R', click: () => platformView?.webContents.reload() },
        { label: '开发者工具', accelerator: process.platform === 'darwin' ? 'Alt+Cmd+I' : 'Ctrl+Shift+I', click: () => platformView?.webContents.toggleDevTools() },
        { type: 'separator' },
        { label: '切换全屏', accelerator: process.platform === 'darwin' ? 'Ctrl+Cmd+F' : 'F11', click: () => { if (mainWin && !mainWin.isDestroyed()) mainWin.setFullScreen(!mainWin.isFullScreen()) } },
      ],
    },
    { role: 'windowMenu' },
  ]))
} else {
  Menu.setApplicationMenu(null)
}

// dev 模式 Dock 图标：打包版由 electron-builder 写死在 App bundle；dev（npm start）
// 默认是 Electron 原子图标——用 setIcon 统一为 ai.svg 生成的新图标。
if (process.platform === 'darwin' && app.dock) {
  app.dock.setIcon(path.join(__dirname, 'assets', 'icon.png'))
}

async function start() {
  // 1. 主窗口先加载本地 loading（静态文件，无需后端）
  createMainWindow(() => {
    platformView?.webContents.loadFile(path.join(__dirname, 'loading.html'))
  })
  // 平台页注入：session 级 preload（所有 frame 生效，host 白名单过滤；Electron 35+
  // registerPreloadScript 取代已废弃的 setPreloads）
  session.defaultSession.registerPreloadScript({
    type: 'frame',
    id: 'aic-remote-preload',
    filePath: path.join(__dirname, 'remote-preload.js'),
  })

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

  // browser 壳通道与 Go provider 注册（失败不影响主流程：该 host 无 browser 能力）
  setupBrowserProvider()

  setStep('正在读取配置…')
  const cfg = await getLocalConfig()
  if (cfg && cfg.host) host = cfg.host
  allowedHostsCache = computeAllowedHosts(host)
  // 默认打开地址：host + home_path（默认 /；非法值回退 /）
  let homePath = (cfg && cfg.home_path) || '/'
  if (typeof homePath !== 'string' || !homePath.startsWith('/') || homePath.startsWith('//')) homePath = '/'

  setStep('正在检测平台 ' + host + ' …')
  const reachable = await probeRoot(host)

  // 3. 跳转：平台可达 → host + homePath；否则停留 loading 提示 + 独立本地配置窗口（首配）
  if (reachable) {
    loadMain(host.replace(/\/+$/, '') + homePath)
  } else {
    setStep('平台不可达——请在「本地配置」窗口中检查地址后重试')
    openSettings()
  }

  try {
    createTray()
  } catch (err) {
    console.error('[tray] create failed:', err.message)
  }
}

// ---- 内置 cua-driver（scripts/sync-cua.mjs 同步 vendor/cua → resources/cua） ----
// 固定版本随包分发：macOS 指向 CuaDriver.app（签名/公证原样，TCC 授权归
// com.trycua.driver），win/linux 指向裸二进制。未同步时不注入——Go 后端
// findCuaDriver 回落系统安装路径（用户自装 cua-driver 仍可用）。
function cuaEnv() {
  const plat = process.platform === 'darwin' ? 'darwin' : process.platform === 'win32' ? 'win32' : 'linux'
  const root = app.isPackaged
    ? path.join(process.resourcesPath, 'cua', plat)
    : path.join(__dirname, 'vendor', 'cua', plat)
  if (process.platform === 'darwin') {
    const appPath = path.join(root, 'CuaDriver.app')
    const bin = path.join(appPath, 'Contents', 'MacOS', 'cua-driver')
    if (!fs.existsSync(bin)) return {}
    return { CUA_DRIVER_PATH: bin, CUA_DRIVER_APP: appPath }
  }
  const bin = path.join(root, process.platform === 'win32' ? 'cua-driver.exe' : 'cua-driver')
  return fs.existsSync(bin) ? { CUA_DRIVER_PATH: bin } : {}
}

// ---- 启动子进程与握手 ----
function spawnBackend() {
  return new Promise((resolve) => {
    const portFile = path.join(app.getPath('userData'), 'aic-port.json')
    try { fs.rmSync(portFile, { force: true }) } catch (e) { /* 忽略 */ }
    backend = spawn(backendBin, [], {
      // AIC_NODE_BIN：Electron 二进位路径，后端 cua run 以 ELECTRON_RUN_AS_NODE=1
      // 将其当纯 node 运行时跑脚本（三平台 Electron 包自带，零新增依赖）。
      // cuaEnv()：内置 cua-driver 路径注入（缺失时空对象，回落系统探测）。
      env: { ...process.env, AIC_PORT_FILE: portFile, AIC_DEVICE_TYPE: 'desktop', AIC_NODE_BIN: process.execPath, ...cuaEnv() },
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

// ---- browser 壳通道（共享插件 browser core + Electron CDP 适配器） ----
// browser-tool.js 是 ESM（core 同源 ESM），从 CJS 主进程动态 import 装载。
// 注册成功后 Go 后端把 browser 加入 caps 并重发；exec browser 请求经
// 127.0.0.1 TCP 换行 JSON 通道转发回本进程执行（Go libs/host/register.go）。
async function setupBrowserProvider() {
  const maxAttempts = 3
  for (let attempt = 1; attempt <= maxAttempts; attempt++) {
    try {
      const { startBrowserServer } = await import('./browser-tool.mjs')
      const { port, token, adapter } = await startBrowserServer({
        host: {
          win: mainWin,
          getViewport: async () => {
            const cfg = await getLocalConfig()
            return { width: cfg?.browser_width, height: cfg?.browser_height }
          },
          onFrame: (frame) => {
            if (platformReady()) platformView.webContents.send('native:frame', frame)
          },
          // 标签集变化 → 全量推平台页（渲染器以 getState 为权威源，事件只做增量提醒）
          onChanged: (st) => {
            try {
              if (platformView && !platformView.webContents.isDestroyed()) {
                platformView.webContents.send('native:changed', st)
              }
            } catch (_) { /* 渲染器重建中 */ }
          },
        },
        log: (f, ...a) => console.log('[browser]', f, ...a),
      })
      aiBrowser = { adapter }
      const r = await fetch(`http://127.0.0.1:${localPort}/api/provider/register`, {
        method: 'POST',
        headers: { 'x-aic-code': localCode, 'Content-Type': 'application/json' },
        body: JSON.stringify({ command: 'browser', addr: `127.0.0.1:${port}`, token }),
        signal: AbortSignal.timeout(5000),
      })
      const d = await r.json().catch(() => ({}))
      if (!r.ok) throw new Error(d.message || `HTTP ${r.status}`)
      console.log('[browser] provider registered (channel 127.0.0.1:%d)', port)
      return
    } catch (e) {
      console.error(`[browser] provider setup failed (attempt ${attempt}/${maxAttempts}):`, e.message)
      if (attempt < maxAttempts) await new Promise((r) => setTimeout(r, 3000))
    }
  }
}

// ---- 白名单（注入与 IPC 校验共用） ----
let allowedHostsCache = [DEFAULT_HOST.replace(/^https?:\/\//, ''), ...LEGACY_HOSTS]
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
  for (const h of LEGACY_HOSTS) {
    if (!list.includes(h)) list.push(h)
  }
  return list
}

// 记住目标平台 host：改地址后新平台页也要注入 remote-preload（白名单缓存启动时
// 只按配置 host 算一次，设置窗口确认打开新地址时增量并入）。
function rememberPlatformHost(url) {
  try {
    const h = new URL(url).host
    if (h && !allowedHostsCache.includes(h)) allowedHostsCache.push(h)
  } catch (e) { /* 非法 URL 忽略 */ }
}

// 校验 IPC 调用方 frame 是否平台白名单（host ∈ 配置 host / 默认与旧平台域名）
function isPlatformFrame(event) {
  try {
    const h = new URL(event.senderFrame.url).host
    return allowedHostsCache.includes(h)
  } catch (e) {
    return false
  }
}

// 校验调用方是否本地设置页（127.0.0.1:<本地服务端口>，精确 host:port——平台开发态
// 常用 localhost:4000 同机不同端口，按 hostname 判会把平台页误当本地页）
function isLocalFrame(event) {
  if (!localPort) return false
  try {
    const u = new URL(event.senderFrame.url)
    return u.host === `127.0.0.1:${localPort}`
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
  for (const w of [petWin, platformView]) {
    if (w && !(w.isDestroyed ? w.isDestroyed() : w.webContents.isDestroyed())) { w.webContents.send('pet:cmd', { action: 'wake' }); delivered = true }
  }
  reply(delivered ? { ok: true } : { ok: false, error: 'no window alive' })
}

// ---- browser 展示桥：平台提供展示区域，浏览器运行视口独立固定 ----

const tabCtl = () => aiBrowser?.adapter?.tabControl || null
const emptyState = () => ({ tabs: [], activeTabId: null })

// 平台页整帧跳转/崩溃 → 停止画面推送（不影响后台 browser；
// getState 是权威源，事件丢了无所谓）
function resetNativeContent() {
  browserInputFocused = false
  platformView?.webContents.setIgnoreMenuShortcuts(false)
  tabCtl()?.applyLayout({ visible: false })
}

// Physical keyboard focus stays in the platform viewer, so its existing layout
// shortcuts receive native events (including leader releases) without handoff.
let leaderMods = []

function platformReady() {
  if (!platformView || platformView.webContents.isDestroyed()) return false
  try {
    return allowedHostsCache.includes(new URL(platformView.webContents.getURL()).host)
  } catch (_) {
    return false
  }
}

// 本地配置 = 独立设置窗口（系统边框）。不依赖平台页/主窗状态——平台或主窗异常
// 时仍可打开改基本配置；单例（已开则 show+focus），关闭即销毁、重开重载。
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

// 绑定/解绑完成后：若本地配置窗口开着，原地重载（等价「重新打开这个界面」，
// 展示 pod 侧最新凭证/连接状态）——配置页只在打开时取数，需要主进程这一推。
function reloadSettingsIfOpen() {
  try {
    if (settingsWin && !settingsWin.isDestroyed()) settingsWin.webContents.reload()
  } catch (_) { /* 窗口销毁竞态 */ }
}

// ---- IPC ----
function registerIpc() {
  // 白名单下发（remote-preload 顶层 sendSync）：平台 host 列表
  ipcMain.on('allowed:hosts', (e) => {
    e.returnValue = allowedHostsCache
  })

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
      // 平台页完成绑定/解绑 → 本地配置窗口（若开着）原地重载
      if (m === 'bind' || m === 'unbind') reloadSettingsIfOpen()
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

  // ---- native:* 原生内容池桥（仅平台白名单；docs §3） ----
  const validTabUrl = (u) => /^https?:\/\//i.test(u) || u === 'about:blank'

  ipcMain.handle('native:state', (e) => {
    if (!isPlatformFrame(e)) throw new Error('forbidden')
    return tabCtl()?.getState() || emptyState()
  })

  ipcMain.handle('native:tab-create', async (e, url) => {
    if (!isPlatformFrame(e) || !aiBrowser) throw new Error('forbidden')
    const u = String(url || '').trim()
    if (u && !validTabUrl(u)) throw new Error('invalid url')
    await aiBrowser.adapter.tabs.create({ windowId: mainWin.id, url: u || 'about:blank' })
    return tabCtl()?.getState() || emptyState()
  })

  ipcMain.handle('native:tab-close', async (e, id) => {
    if (!isPlatformFrame(e) || !aiBrowser) throw new Error('forbidden')
    await aiBrowser.adapter.tabs.remove(Number(id))
    return tabCtl()?.getState() || emptyState()
  })

  ipcMain.handle('native:tab-activate', (e, id) => {
    if (!isPlatformFrame(e)) throw new Error('forbidden')
    tabCtl()?.setActive(id)
    return tabCtl()?.getState() || emptyState()
  })

  ipcMain.handle('native:tab-navigate', async (e, id, url) => {
    if (!isPlatformFrame(e) || !aiBrowser) throw new Error('forbidden')
    const u = String(url || '').trim()
    if (!validTabUrl(u)) throw new Error('invalid url')
    await aiBrowser.adapter.tabs.update(Number(id), { url: u })
    return tabCtl()?.getState() || emptyState()
  })

  // 展示布局：只影响画面推送与输入映射，不修改 browser bounds。
  ipcMain.handle('native:layout', (e, st) => {
    if (!isPlatformFrame(e)) return false
    tabCtl()?.applyLayout({ rect: st?.rect ?? null, visible: !!(st && st.visible), refresh: st?.refresh === true })
    return true
  })

  // The platform owns physical focus and IME. Input is mapped into the fixed
  // browser viewport; no background browser window is ever shown or focused.
  for (const kind of ['mouse', 'wheel', 'key', 'text', 'edit', 'reset']) {
    ipcMain.on('native:' + kind, (e, payload) => {
      if (!isPlatformFrame(e)) return
      if (kind === 'key' && leaderHit(payload, leaderMods)) return
      Promise.resolve().then(() => tabCtl()?.input?.[kind](payload)).catch(() => {})
    })
  }
  ipcMain.on('native:input-focus', (e, focused) => {
    if (!isPlatformFrame(e)) return
    browserInputFocused = !!focused
    if (!browserInputFocused) e.sender.setIgnoreMenuShortcuts(false)
  })
  ipcMain.on('native:frame-ack', (e, seq) => {
    if (isPlatformFrame(e)) tabCtl()?.acknowledgeFrame(seq)
  })

  // 页面仍是快捷键唯一源；leader 校验防止布局命令被误投给 browser。
  // native:focus 保留桥兼容性（launcher 等动作）。
  ipcMain.handle('native:leader', (e, mods) => {
    if (!isPlatformFrame(e)) return false
    leaderMods = normMods(mods)
    return true
  })
  ipcMain.handle('native:focus', (e) => {
    if (!isPlatformFrame(e)) return false
    if (platformView && !platformView.webContents.isDestroyed()) platformView.webContents.focus()
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
    rememberPlatformHost(u)
    loadMain(u)
    // 不关闭配置窗口：配置页内任何操作（保存/获取）都不得让配置页消失
    return true
  })

  // 本地配置窗口关闭（设置页「关闭」按钮触发）
  ipcMain.handle('settings:close', (e) => {
    if (!isLocalFrame(e)) return false
    closeSettings()
    return true
  })
}

// ---- Windows：Alt+Space 抢占（应用内 leader+space = launcher，系统默认弹窗口菜单） ----
// Electron 33 的窗口过程把 Alt+Space 交给 DefWindowProc 弹系统菜单（还原/最小化/最大化/
// 关闭），WM_SYSKEYDOWN 也被系统消费——页面收不到 keydown，应用内快捷键失效、菜单乱弹。
// 处理：窗口聚焦期间 RegisterHotKey 抢占该组合（系统不再弹菜单），命中后把 Space 键回注
// 平台页——页面按既有 keymap 决定动作（默认 launcher，改键位也跟随）。失焦即注销，不长期
// 占用系统热键；注册失败（被 PowerToys Run 等占用）只告警，维持现状。
const ALT_SPACE = 'Alt+Space'
let altSpaceHeld = false

function syncAltSpace() {
  if (process.platform !== 'win32') return
  const focused = !!mainWin && !mainWin.isDestroyed() && (mainWin.isFocused() || (!!petWin && !petWin.isDestroyed() && petWin.isFocused()))
  if (focused && !altSpaceHeld) {
    altSpaceHeld = globalShortcut.register(ALT_SPACE, onAltSpace)
    if (!altSpaceHeld) console.warn('[hotkey] Alt+Space 注册失败（已被其它应用占用？）')
  } else if (!focused && altSpaceHeld) {
    globalShortcut.unregister(ALT_SPACE)
    altSpaceHeld = false
  }
}

// 命中：主窗聚焦 → 回注 Space（alt 修饰）走页面 keymap；桌宠聚焦 → 仅吞掉不弹菜单
function onAltSpace() {
  if (!mainWin || mainWin.isDestroyed() || !mainWin.isFocused()) return
  const wc = platformView && !platformView.webContents.isDestroyed() ? platformView.webContents : null
  if (!wc) return
  wc.sendInputEvent({ type: 'keyDown', keyCode: 'Space', modifiers: ['alt'] })
  wc.sendInputEvent({ type: 'keyUp', keyCode: 'Space', modifiers: ['alt'] })
}

// ---- 窗口 ----
function createMainWindow(init) {
  mainWin = new BaseWindow({
    width: 1280,
    height: 800,
    minWidth: 800,
    minHeight: 600,
    frame: false,
    show: false,
    // fullscreen 键只在需要启动即全屏时传 true——显式传 false 会让 macOS frameless
    // BaseWindow 的 setFullScreen(true) 永久失效（Electron 33.4.11 最小复现 2/2：
    // 创建时 fullscreen:false → 之后 setFullScreen 恒 no-op；不传该键则正常）
    ...(startFullscreen ? { fullscreen: true } : {}),
    backgroundColor: '#1b2a3a',
  })

  // 平台页（恒占满 contentView 且恒最顶；session 级 preloads 自动注入 remote-preload.js）
  platformView = new WebContentsView({
    webPreferences: {
      contextIsolation: true,
      nodeIntegration: false,
      sandbox: true,
    },
  })
  mainWin.contentView.addChildView(platformView)
  // v2 反转模型：平台页背景透明——内容区"洞"由页面 mask 挖除，洞底即下层原生视图
  //（docs §1/§4；页面侧用 mask 而非 clip-path，clip-path 会让采样元素不可命中）
  platformView.setBackgroundColor('#00000000')
  // 平台页 zoom 硬钉 1（Electron 44 setZoomMode）：nativeWin 坐标契约
  //（CSS px = DIP，zoom 恒等映射）由框架保证，不依赖“页面未启用 zoom”的约定
  platformView.webContents.setZoomMode('disabled')
  // v2：洞内输入由页面命中判定后经 IPC（native:mouse / native:wheel）转发（docs §6）
  // 平台页 target=_blank → 系统浏览器
  platformView.webContents.setWindowOpenHandler(({ url }) => {
    if (/^https?:\/\//.test(url)) shell.openExternal(url)
    return { action: 'deny' }
  })
  // 平台页整帧跳转（非 SPA 内跳转）/ 渲染进程崩溃 → 原生内容复位隐藏态
  platformView.webContents.on('did-start-navigation', (_e, _url, isInPlace, isMainFrame) => {
    if (isMainFrame && !isInPlace) resetNativeContent()
  })
  platformView.webContents.on('render-process-gone', resetNativeContent)
  platformView.webContents.on('before-input-event', (_event, input) => {
    // Only edit accelerators belong to the offscreen page. Keep application
    // shortcuts (Quit, Hide, reload, DevTools...) available while viewing it.
    const edit = (input.meta || input.control) && !input.alt && /^[acvxyz]$/i.test(input.key)
    platformView.webContents.setIgnoreMenuShortcuts(browserInputFocused && edit)
  })

  layoutMain()
  mainWin.on('resize', layoutMain)
  mainWin.on('focus', syncAltSpace)
  mainWin.on('blur', syncAltSpace)

  // BaseWindow 无 ready-to-show（BrowserWindow 专属）：内容 view 创建即直接显示
  if (startFullscreen) mainWin.setFullScreen(true)
  mainWin.show()
  syncAltSpace()
  mainWin.on('close', (e) => {
    if (!quitting) {
      e.preventDefault()
      mainWin.hide()
    }
  })
  if (init) init()
}

// ---- 主窗口布局 ----
// 这里只改变平台页的尺寸。各 browser 视口永远不跟随主窗口。
function layoutMain() {
  if (!mainWin || mainWin.isDestroyed()) return
  const [w, h] = mainWin.getContentSize()
  platformView?.setBounds({ x: 0, y: 0, width: w, height: h })
}

function loadMain(url) {
  if (!platformView) return
  platformView.webContents.loadURL(url)
  keepWorkerAlive(url) // 保活窗口跟随平台地址：持 nc SharedWorker 端口，平台页刷新不再销毁 worker/WS
  mainWin?.show()
  mainWin?.focus()
}

// ---- worker 保活窗口 ----
// nc 通道由 SharedWorker 持有（ui/assets/libs/nc.js → nc.worker.js，同源同 URL 共享同一实例），
// 其生命周期 = 最后一个客户端端口断开即销毁。桌面端平台页是唯一客户端：平台页刷新（Cmd+R）
// 会让 worker 连同 WS 一起销毁、整条 nc 通道冷启动。本窗口常驻加载 {base}/worker-keep.html
//（该页仅 new SharedWorker 同 URL 并持端口），使 worker/WS 跨平台页刷新保持存活。
// 部署依赖：平台需已提供该静态页（aic/ui/worker-keep.html，go:embed——先重启平台再重启本应用）。
function keepWorkerAlive(base) {
  // 保活页固定在平台根（go:embed 静态页位于根路径）——拼在 home_path 之后
  //（非 / 时）会 404 且 HTTP 错误不触发 did-fail-load，保活静默失效
  const url = new URL('/worker-keep.html', base).href
  keepExpectedUrl = url
  if (keepWin && !keepWin.isDestroyed()) {
    if (keepWin.webContents.getURL() !== url) keepWin.loadURL(url)
    return
  }
  keepWin = new BrowserWindow({
    show: false,
    skipTaskbar: true,
    webPreferences: {
      contextIsolation: true,
      nodeIntegration: false,
      sandbox: true,
      backgroundThrottling: false, // 隐藏常驻页不节流（端口活性本不依赖计时器，防御性开关）
    },
  })
  keepWin.on('closed', () => { keepWin = null })
  // 渲染进程崩溃 → 原地重载恢复（保活窗口消亡 = worker 回到“刷新即死”）
  keepWin.webContents.on('render-process-gone', () => {
    try { if (keepWin && !keepWin.isDestroyed()) keepWin.webContents.reload() } catch (_) { /* 忽略 */ }
  })
  // 网络级加载失败（如平台暂不可达）→ 10s 后重试（-3=ERR_ABORTED 除外，属正常打断）；
  // 重试前比对当前期望地址——等待期间可能经设置窗切换平台地址，防旧定时器把保活窗拉回旧平台
  keepWin.webContents.on('did-fail-load', (_e, code, _desc, failedUrl, isMainFrame) => {
    if (isMainFrame && code !== -3) setTimeout(() => {
      if (keepWin && !keepWin.isDestroyed() && keepExpectedUrl === failedUrl) keepWin.loadURL(failedUrl)
    }, 10000)
  })
  keepWin.loadURL(url)
}

function setStep(text) {
  platformView?.webContents.executeJavaScript(`window.__setStep && window.__setStep(${JSON.stringify(text)})`).catch(() => { })
}

// ---- 托盘：win 右下角 / mac 菜单栏 ----
function createTray() {
  tray = new Tray(trayIcon)
  tray.setToolTip('AIC Desktop')
  tray.setContextMenu(Menu.buildFromTemplate([
    { label: '打开', click: () => focusMain() },
    { label: '本地配置', click: () => openSettings() },
    { label: '打开配置目录', click: () => openConfigDir() },
    { type: 'separator' },
    { label: '退出', click: () => { quitting = true; app.quit() } },
  ]))
  tray.on('click', () => focusMain())
}

// Go 后端配置根目录（os.UserConfigDir()/aic：config.yaml / aic.log / browser 状态同根）。
// Electron 侧按平台推导同一路径，供托盘「打开配置目录」用系统文件管理器打开。
function configDir() {
  if (process.platform === 'darwin') return path.join(app.getPath('home'), 'Library', 'Application Support', 'aic')
  if (process.platform === 'win32') return path.join(process.env.APPDATA || path.join(app.getPath('home'), 'AppData', 'Roaming'), 'aic')
  return path.join(process.env.XDG_CONFIG_HOME || path.join(app.getPath('home'), '.config'), 'aic')
}

function openConfigDir() {
  const dir = configDir()
  try { fs.mkdirSync(dir, { recursive: true }) } catch (e) { /* 建目录失败由 openPath 报错兜底 */ }
  shell.openPath(dir).then((err) => { if (err) console.error('[tray] open config dir failed:', err) })
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
  petWin.on('focus', syncAltSpace)
  petWin.on('blur', syncAltSpace)
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
  syncAltSpace() // 桌宠销毁后主窗恢复焦点前的兜底对账
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
