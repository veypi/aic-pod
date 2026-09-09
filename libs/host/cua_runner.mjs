// cua_runner.mjs — cua run 的脚本宿主（go:embed 进 aic-pod，由 cua_run.go 落盘启动）。
//
// 运行方式：node cua_runner.mjs <addr> <token>，用户脚本全文从 stdin 读入。
// 协议：与 Go 侧本地 TCP 桥（127.0.0.1，仅脚本生命周期内存活）换行 JSON——
//   请求  {"id":N,"token":"...","tool":"<mcp 工具或 __ 前缀内建>","args":{...}}
//   应答  {"id":N,"ok":true,"text":"...","structured":{...}} 或 {"id":N,"error":"..."}
// 用户脚本以 AsyncFunction('cua','console',code) 执行：支持 await/return，
// 无 import（自动化脚本无外部依赖；宿主文件读写可直接 require node:fs）。
// 全部 cua.* 调用经桥转发到 Go 侧持久 MCP 连接（串行、浏览器绑定/token 缓存保留），
// 步骤间零网络往返。每步记 transcript；未捕获异常即终止并带回已执行步骤。
// 结果输出协议：stdout 末尾一行 `__CUA_RESULT__<json>`（Go 侧按标记切取）。

import net from 'node:net'

const [addr, token] = process.argv.slice(2)
const sep = addr.lastIndexOf(':')
const host = addr.slice(0, sep)
const port = Number(addr.slice(sep + 1))

const MAX_STEPS = 500
const steps = [] // transcript: {i, op, brief, ms, ok, err?}
let nextId = 1
let platform = 'darwin'
let session = ''
const pending = new Map()

// ---- stdin（用户脚本全文） ----
function readStdin() {
  return new Promise((resolve, reject) => {
    let buf = ''
    process.stdin.setEncoding('utf8')
    process.stdin.on('data', (d) => { buf += d })
    process.stdin.on('end', () => resolve(buf))
    process.stdin.on('error', reject)
  })
}

// ---- 桥连接 ----
const sock = net.connect(port, host)
let recvBuf = ''
sock.setEncoding('utf8')
sock.on('data', (chunk) => {
  recvBuf += chunk
  for (;;) {
    const nl = recvBuf.indexOf('\n')
    if (nl < 0) break
    const line = recvBuf.slice(0, nl).trim()
    recvBuf = recvBuf.slice(nl + 1)
    if (!line) continue
    let resp
    try { resp = JSON.parse(line) } catch { continue }
    const p = pending.get(resp.id)
    if (!p) continue
    pending.delete(resp.id)
    if (resp.error) p.reject(new Error(resp.error))
    else p.resolve(resp)
  }
})
sock.on('error', (e) => {
  for (const [, p] of pending) p.reject(new Error('bridge: ' + e.message))
  pending.clear()
})

function send(obj) {
  return new Promise((resolve, reject) => {
    sock.write(JSON.stringify(obj) + '\n', (e) => (e ? reject(e) : resolve()))
  })
}

// rawRpc 无步数记账（__hello 等内建握手用）。
function rawRpc(tool, args) {
  const id = nextId++
  return new Promise((resolve, reject) => {
    pending.set(id, { resolve, reject })
    send({ id, token, tool, args: args || {} }).catch((e) => {
      pending.delete(id)
      reject(e)
    })
  })
}

function briefStr(v, cap) {
  let s
  try { s = JSON.stringify(v) } catch { s = String(v) }
  if (s && s.length > cap) s = s.slice(0, cap) + '…'
  return s
}

// rpc 业务调用：步数记账 + transcript 记录；错误抛 JS 异常（脚本可 try/catch）。
async function rpc(op, tool, args, brief) {
  stepGuard()
  const i = steps.length + 1
  const t0 = Date.now()
  try {
    const resp = await rawRpc(tool, args)
    steps.push({ i, op, brief: brief || briefStr(args, 160), ms: Date.now() - t0, ok: true })
    return resp
  } catch (e) {
    steps.push({ i, op, brief: brief || briefStr(args, 160), ms: Date.now() - t0, ok: false, err: String(e && e.message || e) })
    throw e
  }
}

function stepGuard() {
  if (steps.length >= MAX_STEPS) {
    throw new Error(`step limit ${MAX_STEPS} exceeded`)
  }
}

// ---- 目标绑定（target 后动作免填 pid/window） ----
const bind = { pid: 0, window: 0 }

function targetArgs(extra) {
  const a = Object.assign({}, extra)
  // scope=desktop 走真实桌面指针路由，驱动禁止与 pid/window 同用（click /
  // move_cursor schema）——此时不注入绑定目标。
  if (a.scope === 'desktop') return a
  if (bind.pid && a.pid === undefined) a.pid = bind.pid
  if (bind.window && a.window_id === undefined) a.window_id = bind.window
  return a
}

// optArgs 合并动作选项：{delivery:'foreground'|'background', scope:'desktop'|'window',
// 其余键原样透传驱动参数（count/debug_image_out/from_zoom/action…）}。
// 默认不传 = 驱动后台精确路由（AX 语义 → browser/CDP → 窗口本地指针 → PID 键盘 → 拒绝）；
// delivery:'foreground' = 真实全局键盘事件（IME 穿透，会窃取前台焦点）。
// scope:'desktop' = 真实物理指针（会动用户鼠标），坐标为桌面截图物理像素。用途：目标 app
// 的原生模态菜单只认真实指针点击（2026-09-09 Blender 实测：窗口本地指针点不中 Add 菜单项），
// 以及需要先摆好光标位置再按相对位置操作的场景。
function optArgs(args, opts) {
  if (!opts) return args
  if (opts.delivery) args.delivery_mode = opts.delivery
  if (opts.scope) args.scope = opts.scope
  // 其余键原样透传（驱动参数名），合法性由驱动 schema 终审。
  for (const k of Object.keys(opts)) {
    if (k === 'delivery' || k === 'scope') continue
    args[k] = opts[k]
  }
  return args
}

// 元素定位参数：element_token 优先，无 token 用 frame 中心坐标。
function elementArgs(el) {
  if (typeof el === 'number') {
    throw new Error('elementArgs: number 需走坐标分支')
  }
  if (!el || typeof el !== 'object') throw new Error('click 目标是元素对象或坐标')
  if (el.element_token) return { element_token: el.element_token }
  const f = el.frame
  if (f && (f.w || f.h || f.x || f.y)) {
    return { x: f.x + (f.w || 0) / 2, y: f.y + (f.h || 0) / 2 }
  }
  throw new Error('元素既无 element_token 也无 frame（可能是文本节点，无行动性）')
}

// click 目标归一：click(x,y) | click([x,y]) | click(el) | click({x,y})
function clickArgs(a, b) {
  if (typeof a === 'number') return { x: a, y: b }
  if (Array.isArray(a)) return { x: a[0], y: a[1] }
  if (a && typeof a === 'object') {
    if (a.x !== undefined) return { x: a.x, y: a.y }
    return elementArgs(a)
  }
  throw new Error('click 需要坐标或元素对象')
}

// snapshot 应答包装：挂 find/findAll 模糊查找。
function matchEls(elements, q) {
  const test = typeof q === 'string'
    ? (s) => s.toLowerCase().includes(q.toLowerCase())
    : (s) => q.test(s)
  return (elements || []).filter((e) => {
    const label = e.label || e.value || e.description || ''
    return label && test(label)
  })
}

function wrapSnap(structured) {
  const s = structured || {}
  s.find = (q) => matchEls(s.elements, q)[0] || null
  s.findAll = (q) => matchEls(s.elements, q)
  return s
}

// imeEnsure：键盘类动作前确保系统输入法处于英文布局（Go 侧检测/切换，
// 已是英文时零开销跳过）。中文 IME 会把组合键（如 Shift+A）当输入法切换
// 吃掉——2026-09-09 Blender 实测搜狗拼音下 Shift+A 退化成裸 a。
// 失败不阻断动作：按键是否真正生效由动作结果暴露。
async function imeEnsure() {
  try { await rawRpc('__ime', {}) } catch { /* 护栏失败不阻断 */ }
}

// hasNonASCII：中文/日文等文本经逐键合成会被 IME 吞或转候选，改走剪贴板。
function hasNonASCII(s) {
  return /[^\x00-\x7F]/.test(s)
}

// ---- cua API（与单指令子命令一一对应） ----
const cua = {
  // 感知
  apps: async () => (await rpc('apps', 'list_apps', {})).structured,
  windows: async (pid) => (await rpc('windows', 'list_windows', pid ? { pid } : {})).structured,
  async snapshot(opts) {
    const o = opts || {}
    const args = targetArgs({})
    if (o.pid) args.pid = o.pid
    if (o.window) args.window_id = o.window
    if (o.png) args.png = true
    if (!args.pid || !args.window_id) throw new Error('snapshot 需先 target({pid,window}) 或显式传 {pid,window}')
    const resp = await rpc('snapshot', '__snapshot', args)
    return wrapSnap(resp.structured)
  },

  // 目标
  async launch(name, urls) {
    const args = { name }
    if (urls && urls.length) args.urls = urls
    return (await rpc('launch', 'launch_app', args, name)).structured
  },
  async target(t) {
    if (!t || !t.pid) throw new Error('target({pid, window?}) 需要 pid')
    bind.pid = t.pid
    bind.window = t.window || 0
    if (!bind.window) {
      const resp = await rawRpc('list_windows', { pid: bind.pid })
      const wins = (resp.structured && resp.structured.windows) || []
      let first = 0
      for (const w of wins) {
        if (!w.window_id) continue
        if (!first) first = w.window_id
        if (w.is_on_screen) { first = w.window_id; break }
      }
      bind.window = first
    }
    steps.push({ i: steps.length + 1, op: 'target', brief: `pid=${bind.pid} window=${bind.window}`, ms: 0, ok: true })
    return { pid: bind.pid, window: bind.window }
  },

  // 动作（均可带末参 opts={delivery,scope}）
  click: async (a, b, opts) => rpc('click', 'click', targetArgs(optArgs(clickArgs(a, b), opts))),
  dclick: async (a, b, opts) => rpc('dclick', 'double_click', targetArgs(optArgs(clickArgs(a, b), opts))),
  rclick: async (a, b, opts) => rpc('rclick', 'right_click', targetArgs(optArgs(clickArgs(a, b), opts))),
  type: async (text, opts) => {
    const s = String(text)
    // 非 ASCII：不走逐键合成（IME 会吞/转候选），改走剪贴板粘贴
    if (hasNonASCII(s)) return cua.paste(s, opts)
    await imeEnsure()
    return rpc('type', 'type_text', targetArgs(optArgs({ text: s }, opts)), briefStr(s, 80))
  },
  async paste(text, opts) {
    await rpc('clipboard.write', 'clipboard_write', { text: String(text) }, briefStr(text, 80))
    await sleep(120)
    const mod = platform === 'darwin' ? 'cmd' : 'ctrl'
    return cua.hotkey(mod + '+v', opts)
  },
  key: async (name, opts) => {
    await imeEnsure()
    return rpc('key', 'press_key', targetArgs(optArgs({ key: name }, opts)), name)
  },
  // hotkey 走 press_key+modifiers（hotkey 工具对 Blender 等原生 app 会丢修饰键：
  // cmd+a 退化成裸 'a'、ctrl+n 退化成 'n'；press_key 的 modifiers 数组为实测可靠
  // 路径——2026-09-09 另一产品的 Blender 记录中 super+a / ctrl+shift+s 均生效）。
  hotkey: async (combo, opts) => {
    const parts = String(combo).split('+')
    if (parts.length < 2) throw new Error('hotkey 需要组合键（如 cmd+v）')
    const args = { key: parts[parts.length - 1], modifiers: parts.slice(0, -1) }
    // cmd/ctrl 组合键一般不被 IME 拦截，但 Shift+X 类会被（搜狗把 Shift 当
    // 中英切换）——统一走护栏
    await imeEnsure()
    return rpc('hotkey', 'press_key', targetArgs(optArgs(args, opts)), combo)
  },
  scroll: async (direction, amount, opts) =>
    rpc('scroll', 'scroll', targetArgs(optArgs(amount ? { direction, amount } : { direction }, opts)), direction),
  drag: async (x1, y1, x2, y2, opts) =>
    rpc('drag', 'drag', targetArgs(optArgs({ from_x: x1, from_y: y1, to_x: x2, to_y: y2 }, opts))),
  move: async (x, y, opts) => rpc('move', 'move_cursor', optArgs({ x, y }, opts || {})),
  // 虚拟光标浮层（daemon 唯一形态，浮层依赖 AppKit 主线程宿主）。浮层纯显示
  // 不交互；与动作同会话执行时驱动自动联动光标动画（主题
  // cua-driver-actions-v2），脚本无需逐步 move。
  cursor: {
    show: async () => rpc('cursor.show', 'set_agent_cursor_enabled', { enabled: true }),
    hide: async () => rpc('cursor.hide', 'set_agent_cursor_enabled', { enabled: false }),
    state: async () => (await rpc('cursor.state', 'get_agent_cursor_state', {})).structured,
    // motion 例：{glide_duration_ms:700, idle_hide_ms:60000, spring:0.72, turn_radius:80}
    motion: async (opts) => rpc('cursor.motion', 'set_agent_cursor_motion', opts || {}),
    move: async (x, y, opts) => rpc('cursor.move', 'move_cursor', optArgs({ x, y }, opts || {})),
  },
  front: async (pid) => {
    const args = { pid: pid || bind.pid }
    if (!args.pid) throw new Error('front 需要 pid（或先 target 绑定）')
    if (bind.window) args.window_id = bind.window
    return rpc('front', 'bring_to_front', args)
  },
  setValue: async (tokenOrEl, value) =>
    rpc('set-value', 'set_value', targetArgs({ element_token: typeof tokenOrEl === 'string' ? tokenOrEl : tokenOrEl.element_token, value: String(value) })),
  menu: async (path) =>
    rpc('menu', 'invoke_menu', targetArgs({ path: String(path).split('>') }), path),
  setFrame: async (x, y, width, height) =>
    rpc('set-frame', 'set_window_frame', targetArgs({ x, y, width, height })),
  clipboardRead: async () => (await rpc('clipboard.read', 'clipboard_read', { include_text: true })).structured,
  clipboardWrite: async (text) => rpc('clipboard.write', 'clipboard_write', { text: String(text) }),

  // 浏览器家族（绑定由 Go 侧注入/存取）
  bprepare: async (opts) => {
    const o = opts || {}
    const args = o.isolated
      ? { profile: { mode: 'isolated_new' }, allow_launch: true }
      : { strategy: { kind: 'existing_profile' }, pid: o.pid, window_id: o.window }
    return (await rpc('bprepare', 'browser_prepare', args)).structured
  },
  browserState: async (opts) =>
    (await rpc('browser-state', 'get_browser_state', Object.assign({ snapshot_format: 'semantic_v2' }, opts))).structured,
  navigate: async (url) => rpc('navigate', 'browser_navigate', { url }, url),
  bclick: async (a, y) => {
    const args = typeof a === 'string' ? { ref: a }
      : (a && typeof a === 'object' && a.ref) ? { ref: a.ref }
      : clickArgs(a, y)
    return rpc('bclick', 'browser_click', args)
  },
  btype: async (ref, text, opts) =>
    rpc('btype', 'browser_type', Object.assign({ ref, text: String(text) }, opts), briefStr(text, 80)),
  bend: async () => rpc('bend', 'end_session', {}),

  // 控制
  sleep: (ms) => sleep(ms),
  log: (msg) => {
    stepGuard()
    steps.push({ i: steps.length + 1, op: 'log', brief: String(msg).slice(0, 500), ms: 0, ok: true })
  },
}

function sleep(ms) {
  return new Promise((r) => setTimeout(r, ms))
}

// console 桥进 transcript（脚本调试输出不落 stdout，保持结果标记干净）。
const consoleBridge = {}
for (const k of ['log', 'info', 'warn', 'error']) {
  consoleBridge[k] = (...xs) => cua.log(xs.map((x) => (typeof x === 'string' ? x : briefStr(x, 300))).join(' '))
}

// ---- 主流程 ----
function finalize(result, err) {
  const payload = { steps, result: result === undefined ? null : result, error: err ? String(err && err.message || err) : '' }
  sock.destroy()
  process.stdout.write('\n__CUA_RESULT__' + JSON.stringify(payload) + '\n')
}

async function main() {
  await new Promise((resolve, reject) => {
    sock.once('connect', resolve)
    sock.once('error', reject)
  })
  const hello = await rawRpc('__hello', {})
  platform = (hello.structured && hello.structured.platform) || 'darwin'
  session = (hello.structured && hello.structured.session) || ''
  const code = await readStdin()
  if (!code.trim()) throw new Error('empty script')
  const AsyncFunction = Object.getPrototypeOf(async function () {}).constructor
  const fn = new AsyncFunction('cua', 'console', code)
  const result = await fn(cua, consoleBridge)
  finalize(result, null)
}

main().then(
  () => process.exit(0),
  (e) => { finalize(null, e); process.exit(0) }, // 脚本失败非进程失败：结果走标记行
)
