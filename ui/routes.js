/*
 * routes.js
 * Copyright (C) 2026 veypi <i@veypi.com>
 *
 * Distributed under terms of the MIT license.
 */

const routes = [
  // ---- 壳页面：首页（iframe 平台页）与设置页共用 default 布局（header 常驻） ----
  { path: "/settings", component: "/page/settings.html", layout: "default" },
  { path: "/", component: "/page/index.html", layout: "default" },
  // ---- 桌宠：无布局，全屏透明 + ai.svg（点击回首页） ----
  { path: "/pet", component: "/page/pet.html" },
];

export default ({ $mod }) => ({
  routes: routes,
  beforeEnter: async (to, from, next) => {
    next();
  },
});
