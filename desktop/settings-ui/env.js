/*
 * env.js — 本地设置页环境（Electron 设置窗口，app://aic 自定义协议）
 *   - 不连本地 HTTP：设置在主进程里落盘（spawn `aic-backend config|bind` 子命令）。
 *   - $mod.$pod.*：设置桥（settings-preload → IPC 'local:api' → 主进程）：
 *       get_config / set_config / bind / unbind / get_status / get_log /
 *       check_host / start / stop
 *     与平台页 window.aicDesktop.api 同一套方法名（main.js 一份实现）。
 *   - $mod.$bus：全局事件总线（shell-nav 导航事件等）。
 */
export default async ($mod) => {
  const base = $mod.scoped || '';

  // 加载 i18n 配置（langs.json 可为空）
  fetch(base + '/langs.json')
    .then((res) => res.json())
    .then((data) => {
      $mod.$i18n.load(data);
    })
    .catch(() => {});

  const bridge = (window.aicDesktop && window.aicDesktop.api) || null;
  const call = (name, args) => {
    if (!bridge) return Promise.reject(new Error('设置桥不可用（请在 AIC Desktop 中打开设置）'));
    return Promise.resolve(bridge(name, args === undefined ? null : args));
  };

  $mod.$pod = {
    get_config: () => call('get_config'),
    set_config: (cfg) => call('set_config', cfg),
    bind: (credential) => call('bind', { credential }),
    unbind: () => call('unbind', {}),
    get_status: () => call('get_status'),
    get_log: () => call('get_log'),
    check_host: (host) => call('check_host', { host }),
    start: () => call('start', {}),
    stop: () => call('stop', {}),
  };
};
