'use strict'

// secure-origin.cjs — 平台 origin 的「安全上下文」判定（纯函数，单测直引）。
//
// 背景：page 端文件能力整体依赖 OPFS（fs 工具 _host=page、page exec 的
// ls/rg/cp/mv/rm、二进制文件上传引用、ppt_studio/drawio/office_studio 等技能），
// 而 OPFS 只在安全上下文可用——明文 http + 非 loopback 主机不是可信来源，
// 浏览器直接不暴露 navigator.storage（isSecureContext=false），
// page_fs.js 的 defaultRootProvider() 因此直接抛
// 「fs {action}: OPFS not available in this browser」。
//
// 桌面壳的处置：进程启动期把这类 origin 提升为安全上下文
// （Chromium: --unsafely-treat-insecure-origin-as-secure）。本模块只回答
// 「这个平台地址需不需要提升」，开关的追加与提示在 main.js。

// loopback 主机：http://localhost / 127.0.0.0/8 / ::1 本就是可信来源，无需提升。
function isLoopbackHost(hostname) {
  const h = String(hostname || '').trim().toLowerCase().replace(/^\[|\]$/g, '')
  if (!h) return false
  if (h === 'localhost' || h.endsWith('.localhost')) return true
  if (h === '::1') return true
  if (/^127\.\d{1,3}\.\d{1,3}\.\d{1,3}$/.test(h)) return true
  return false
}

// 需要提升为安全上下文的 origin（scheme://host[:port]）；无需提升时返回 ''。
// 只认显式 http/https：其余（无 scheme、file:、非法串）一律不处理，
// 不猜协议——猜错会把一个本不需要提升的地址写进开关。
function insecureOriginFor(host) {
  const raw = String(host || '').trim()
  if (!raw) return ''
  let u
  try {
    u = new URL(raw)
  } catch (e) {
    return ''
  }
  if (u.protocol !== 'http:') return '' // https 已是安全上下文
  if (!u.host) return ''
  if (isLoopbackHost(u.hostname)) return '' // loopback 本就可信
  return u.origin
}

// 从 `aic-backend config get` 的 stdout 里取 host。
// logv 的 console 输出走 stdout（带 ANSI 颜色），JSON 是最后一段可解析的对象；
// 逐个 '{' 试起点，避开日志行里的花括号。
function hostFromConfigOutput(text) {
  const s = String(text || '')
  const end = s.lastIndexOf('}')
  if (end < 0) return ''
  for (let i = s.indexOf('{'); i >= 0 && i < end; i = s.indexOf('{', i + 1)) {
    try {
      const v = JSON.parse(s.slice(i, end + 1))
      return String((v && v.host) || '')
    } catch (e) {
      /* 不是 JSON 起点（可能是日志行里的花括号）→ 试下一个 */
    }
  }
  return ''
}

module.exports = { isLoopbackHost, insecureOriginFor, hostFromConfigOutput }
