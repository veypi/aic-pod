'use strict'

// 平台/设置窗来源白名单：preload 注入与 IPC 调用方校验共用。
// 默认 host + 旧域名常驻；配置 host 与设置窗确认的新地址增量并入。
const { SETTINGS_SCHEME } = require('./settings-protocol.cjs')

function computeAllowedHosts(platform, defaultHost, legacyHosts) {
  const list = []
  const add = (u) => {
    try {
      const h = new URL(u).host
      if (h && !list.includes(h)) list.push(h)
    } catch (e) { /* 忽略 */ }
  }
  add(platform)
  add(defaultHost)
  for (const h of legacyHosts) {
    if (!list.includes(h)) list.push(h)
  }
  return list
}

function createHostPolicy({ platform, defaultHost, legacyHosts = [] }) {
  let allowed = computeAllowedHosts(platform, defaultHost, legacyHosts)

  function remember(url) {
    try {
      const h = new URL(url).host
      if (h && !allowed.includes(h)) allowed.push(h)
    } catch (e) { /* 非法 URL 忽略 */ }
  }

  return {
    all: () => allowed,
    replace: (value) => { allowed = computeAllowedHosts(value, defaultHost, legacyHosts) },
    remember,
    isPlatformFrame(event) {
      try {
        return allowed.includes(new URL(event.senderFrame.url).host)
      } catch (e) {
        return false
      }
    },
    isSettingsFrame(event) {
      try {
        const u = new URL(event.senderFrame.url)
        return u.protocol === SETTINGS_SCHEME + ':' && u.hostname === 'aic'
      } catch (e) {
        return false
      }
    },
  }
}

module.exports = { createHostPolicy, computeAllowedHosts }
