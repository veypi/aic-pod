/*
 * env.js — 本地壳页面环境（vigo serve，http://localhost:{port}/?code=xxx）
 *   - code：URL 首次打开注入（desktop/cli 打开壳时携带），存 localStorage，
 *     进程重启后 code 换新 → 新 URL 再次覆盖；同进程内刷新/路由切换从缓存取。
 *   - $mod.$fetch：自动注入 x-aic-code 头，页面代码不再手拼 code。
 *   - $mod.$bus：全局事件总线（shell-nav 导航事件等）。
 */
export default async ($mod, all) => {
  // 加载 i18n 配置（langs.json 可为空）
  fetch($mod.scoped + "/langs.json")
    .then((res) => res.json())
    .then((data) => {
      $mod.$i18n.load(data);
    })
    .catch(() => {});

  // code：URL ?code= → localStorage（每次打开覆盖；重启换新后自动更新）
  const LS_CODE = "aic.code";
  const c = new URLSearchParams(location.search).get("code");
  if (c) {
    try {
      localStorage.setItem(LS_CODE, c);
    } catch (e) {
      /* 隐私模式忽略 */
    }
  }
  const code = () => {
    try {
      return localStorage.getItem(LS_CODE) || "";
    } catch (e) {
      return "";
    }
  };

  $mod.$fetch = async (url, opts = {}) => {
    let fullUrl = url;
    if (opts.params) {
      const qs = new URLSearchParams();
      for (const [k, v] of Object.entries(opts.params)) {
        if (v != null) qs.append(k, v);
      }
      fullUrl += "?" + qs.toString();
    }
    const init = { method: opts.method || "GET", headers: {} };
    const c = code();
    if (c) init.headers["x-aic-code"] = c;
    if (opts.body != null && init.method !== "GET") {
      init.headers["Content-Type"] = "application/json";
      init.body = JSON.stringify(opts.body);
    }
    const r = await window.fetch(fullUrl, init);
    const d = await r.json().catch(() => ({}));
    if (!r.ok) {
      let msg = d.message;
      if (!msg && d.error) msg = d.error;
      throw new Error(msg || `HTTP ${r.status}`);
    }
    return d;
  };
};
