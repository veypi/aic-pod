/* leader-grab.js — leader 键抓取/会话判定（纯逻辑，node:test 覆盖）
 *
 * 背景（设计唯一源 aic/docs/os_native_windows.md §6）：平台页恒最顶且背景透明，
 * 原生内容（AI 标签池）在其下；点击进洞后壳侧 wc.focus() 把键盘
 * 焦点交给原生内容，平台页收不到 keydown——leader 布局快捷键（编排 / launcher /
 * 窗口动作）随之失效。
 *
 * 机制 =「leader 会话焦点交接」（2026-09-13）：
 *   内容 view 上 leader 集合精确命中（leaderHit）→ 壳 preventDefault（该键不进内容）
 *   + 合成"按下"事件转平台页（进入编排）+ 键盘焦点交接平台页；此后物理键由平台页
 *   原生接收（既有 keymap/编排链路零改动），leader 释放（leaderReleased 判定）→
 *   焦点交还来源 view。
 *
 * 为什么不做「逐键吞下并转发」：Chromium 对被处理（handled）的 keyDown 会连带抑制
 * 其后所有 keyUp/char（render_widget_host_impl.cc 的 suppress_events_until_keydown_，
 * 直到下一个新 keyDown 才恢复）——壳侧永远观测不到 leader 释放，编排退出必然卡死
 * （Electron issue #37336 官方确认 intended behavior）。焦点交接后释放是平台页上的
 * 真实事件，该限制自然绕开。
 *
 * 事件口径：input 即 Electron before-input-event 的 input（key/code/修饰布尔/
 * isAutoRepeat/isComposing）。修饰键识别用 code（AltLeft 等，与页面 keymap.js
 * isModifierKey 同集）；修饰键自身标志在 keyDown/keyUp 两端归一（部分平台 keyUp
 * 的自身标志未清，防集合比较漂移）。
 */
'use strict'

const MODS = ['alt', 'ctrl', 'shift', 'meta'] // 与页面 keymap 同口径（ctrl = Electron control）
const MOD_BY_CODE = { Alt: 'alt', Control: 'ctrl', Shift: 'shift', Meta: 'meta' }

// 'AltLeft' → 'alt'；非修饰键 → null
function modOfCode(code) {
  const m = /^(Alt|Control|Shift|Meta)(Left|Right)$/.exec(String(code || ''))
  return m ? MOD_BY_CODE[m[1]] : null
}

// 清洗外部输入（页面 IPC）：仅保留合法修饰键名、去重、小写
function normMods(list) {
  const out = []
  for (const m of Array.isArray(list) ? list : []) {
    const k = String(m || '').toLowerCase()
    if (MODS.includes(k) && !out.includes(k)) out.push(k)
  }
  return out
}

const sameSet = (a, b) => a.length === b.length && a.every((m) => b.includes(m))

// 从 input 求当前修饰键集合；down=true 补本键（keyDown 自身标志可能未含），
// down=false 去本键（keyUp 自身标志可能未清）——两端归一，集合比较恒稳定
function modsOfInput(input, down) {
  const out = []
  if (input.alt) out.push('alt')
  if (input.control) out.push('ctrl')
  if (input.shift) out.push('shift')
  if (input.meta) out.push('meta')
  const self = modOfCode(input.code)
  if (self && down) {
    if (!out.includes(self)) out.push(self)
  } else if (self) {
    const i = out.indexOf(self)
    if (i >= 0) out.splice(i, 1)
  }
  return out
}

// 内容 view 侧：keyDown 且当前修饰键集合精确等于 leader → 返回命中集合；否则 null
// （与页面 keymap.js sameMods 同口径：集合完整相等的那个按下事件是会话进入点）
function leaderHit(input, leader) {
  const type = String(input?.type || '')
  if (type !== 'keyDown' && type !== 'rawKeyDown') return null
  const want = normMods(leader)
  if (!want.length) return null
  const mods = modsOfInput(input, true)
  return sameSet(mods, want) ? mods : null
}

// 平台页侧：keyUp 后 leader 集合不再完整（任一分量抬起）→ true（会话释放）
function leaderReleased(input, leader) {
  if (String(input?.type || '') !== 'keyUp') return false
  const want = normMods(leader)
  if (!want.length) return false
  const mods = modsOfInput(input, false)
  return !want.every((m) => mods.includes(m))
}

// 合成事件载荷 = 平台页 KeyboardEvent 的原料（修饰布尔取归一后的集合）
function keyPayload(input, mods) {
  return {
    type: 'keyDown',
    key: String(input?.key || ''),
    code: String(input?.code || ''),
    altKey: mods.includes('alt'),
    ctrlKey: mods.includes('ctrl'),
    shiftKey: mods.includes('shift'),
    metaKey: mods.includes('meta'),
    repeat: !!input?.isAutoRepeat,
    isComposing: !!input?.isComposing,
  }
}

module.exports = { normMods, modsOfInput, leaderHit, leaderReleased, keyPayload }
