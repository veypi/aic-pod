/*
 * routes.js
 * Copyright (C) 2026 veypi <i@veypi.com>
 *
 * Distributed under terms of the MIT license.
 */

// 本地服务仅保留配置页 /settings（Electron 主窗口/设置窗口/浏览器壳入口）；
// 平台 UI 全部由远端 aic 提供（桌面版 header / pet 等都在平台侧）。
const routes = [
  { path: "/settings", component: "/page/settings.html" },
  { path: "/", redirect: "/settings" },
];

export default ({ $mod }) => ({
  routes: routes,
  beforeEnter: async (to, from, next) => {
    next();
  },
});
