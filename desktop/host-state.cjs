// host-state.cjs — 本机连接状态合成（main.js localStatus 的纯函数部分，单测直引）。
//
// 背景（2026-09-23「重连假成功」修复）：Go 后端把真实连接状态写
// $HOME/.aic/state.json（连接成功 / 断开 / 认证失败 / 重试失败才更新，
// pid 为后端进程）。桌面禁止再用「子进程存活 + key 非空」冒充已连接——
// 换平台未换 key 时 NATS 会在后台无限静默重试，进程一直活着。
function composeLocalStatus({ alive, key, childPid, state, hostname }) {
  // 凭证首段即 host_id（与 protocol.ParseDeviceCredential 同口径：切割点在第一个 '.'）。
  // 兼容旧 4 段（host_id.x.y.z）与现行 5 段复合串（host_id + vbase 四段 credential）；
  // 段数不对（半截串/多段）一律不兑底 host_id——不拿畸形凭证冒充已绑定设备。
  const boundHostID = (credential) => {
    const parts = String(credential || '').trim().split('.')
    if (parts.length !== 4 && parts.length !== 5) return ''
    return parts[0] || ''
  }
  // fresh：状态必须来自当前子进程（pid 核对；子进程句柄缺失时（测试场景）退化为只看存活）。
  const fresh = !!(alive && state && (childPid == null || state.pid === childPid))
  return {
    running: !!alive && !!key,
    connected: !!(fresh && state.connected),
    host_id: (fresh && state.host_id) || boundHostID(key),
    hostname: hostname || '',
    version: (fresh && state.version) || '',
    last_error: fresh ? (state.last_error || '') : '',
    retrying: !!(fresh && state.retrying),
  }
}

module.exports = { composeLocalStatus }
