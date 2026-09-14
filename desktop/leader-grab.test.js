/* leader-grab.test.js — leader 抓取/会话判定用例（node --test desktop/leader-grab.test.js）
 *
 * 覆盖：normMods 清洗 / modsOfInput 两端归一（keyDown 补本键、keyUp 去本键）/
 * leaderHit 集合精确命中（会话进入点）/ leaderReleased 释放判定（任一分量抬起）/
 * keyPayload 载荷。
 */
'use strict'

const test = require('node:test')
const assert = require('node:assert/strict')
const { normMods, modsOfInput, leaderHit, leaderReleased, keyPayload } = require('./leader-grab')

// 构造 before-input-event 的 input 形态
const key = (type, code, opts = {}) => ({
  type,
  code,
  key: opts.key !== undefined ? opts.key : code,
  alt: !!opts.alt,
  control: !!opts.control,
  shift: !!opts.shift,
  meta: !!opts.meta,
  isAutoRepeat: !!opts.repeat,
  isComposing: !!opts.composing,
})

test('normMods：合法名清洗/去重/小写；非法输入回空', () => {
  assert.deepEqual(normMods(['alt']), ['alt'])
  assert.deepEqual(normMods(['ALT', 'ctrl', 'ctrl', 'x']), ['alt', 'ctrl'])
  assert.deepEqual(normMods('alt'), [])
  assert.deepEqual(normMods(null), [])
})

test('modsOfInput：keyDown 补本键、keyUp 去本键（自身标志两端归一）', () => {
  // Alt 按下：标志可能未含自身 → 补上
  assert.deepEqual(modsOfInput(key('keyDown', 'AltLeft'), true), ['alt'])
  assert.deepEqual(modsOfInput(key('keyDown', 'AltLeft', { alt: true }), true), ['alt'])
  // Alt 抬起：标志可能未清 → 去掉
  assert.deepEqual(modsOfInput(key('keyUp', 'AltLeft', { alt: true }), false), [])
  assert.deepEqual(modsOfInput(key('keyUp', 'AltLeft'), false), [])
  // 组合与顺序（alt+shift）
  assert.deepEqual(modsOfInput(key('keyDown', 'AltLeft', { shift: true }), true), ['shift', 'alt'])
  // 非修饰键原样读标志
  assert.deepEqual(modsOfInput(key('keyDown', 'KeyF', { alt: true }), true), ['alt'])
})

test('leaderHit：集合精确命中才进入（含本键若为修饰键）', () => {
  // Alt 按下 = 进入点
  assert.deepEqual(leaderHit(key('keyDown', 'AltLeft'), ['alt']), ['alt'])
  assert.deepEqual(leaderHit(key('keyDown', 'AltLeft', { alt: true }), ['alt']), ['alt'])
  // 命令键（alt 已按住）：也命中（leader 修饰键在聚焦前已按住的兜底路径）
  assert.deepEqual(leaderHit(key('keyDown', 'KeyF', { alt: true }), ['alt']), ['alt'])
  // 集合不等：Shift 同按不进入；无修饰不进入
  assert.equal(leaderHit(key('keyDown', 'AltLeft', { alt: true, shift: true }), ['alt']), null)
  assert.equal(leaderHit(key('keyDown', 'KeyF'), ['alt']), null)
  // keyUp 不进入；leader 为空不进入
  assert.equal(leaderHit(key('keyUp', 'AltLeft'), ['alt']), null)
  assert.equal(leaderHit(key('keyDown', 'AltLeft'), []), null)
  // 多修饰键 leader：Ctrl 单独按下不进入，Alt 补全才进入
  assert.equal(leaderHit(key('keyDown', 'ControlLeft', { control: true }), ['ctrl', 'alt']), null)
  assert.deepEqual(leaderHit(key('keyDown', 'AltLeft', { control: true, alt: true }), ['ctrl', 'alt']), ['alt', 'ctrl'])
})

test('leaderReleased：keyUp 后 leader 集合不再完整才判定释放', () => {
  // Alt 抬起（自身标志已清/未清都释放）
  assert.equal(leaderReleased(key('keyUp', 'AltLeft'), ['alt']), true)
  assert.equal(leaderReleased(key('keyUp', 'AltLeft', { alt: true }), ['alt']), true)
  // 非 leader 键抬起（alt 仍按住）不释放
  assert.equal(leaderReleased(key('keyUp', 'KeyF', { alt: true }), ['alt']), false)
  // Alt 仍按住时 Shift 抬起不释放
  assert.equal(leaderReleased(key('keyUp', 'ShiftLeft', { alt: true }), ['alt']), false)
  // 多修饰键 leader：Ctrl 抬起（Alt 仍按）即释放；Alt 抬起（Ctrl 仍按）亦释放
  assert.equal(leaderReleased(key('keyUp', 'ControlLeft', { alt: true }), ['ctrl', 'alt']), true)
  assert.equal(leaderReleased(key('keyUp', 'AltLeft', { control: true }), ['ctrl', 'alt']), true)
  // keyDown 不判定；leader 为空不判定
  assert.equal(leaderReleased(key('keyDown', 'AltLeft'), ['alt']), false)
  assert.equal(leaderReleased(key('keyUp', 'AltLeft'), []), false)
})

test('keyPayload：合成事件原料（修饰布尔取归一集合）', () => {
  assert.deepEqual(keyPayload(key('keyDown', 'AltLeft', { key: 'Alt', repeat: true }), ['alt']), {
    type: 'keyDown', key: 'Alt', code: 'AltLeft',
    altKey: true, ctrlKey: false, shiftKey: false, metaKey: false,
    repeat: true, isComposing: false,
  })
  assert.deepEqual(keyPayload(key('keyDown', 'KeyF', { alt: true, key: 'f' }), ['alt']), {
    type: 'keyDown', key: 'f', code: 'KeyF',
    altKey: true, ctrlKey: false, shiftKey: false, metaKey: false,
    repeat: false, isComposing: false,
  })
})
