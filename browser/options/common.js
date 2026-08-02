/**
 * common.js — options 多页共享：页面骨架（顶栏 + 侧栏）渲染 + 连接状态轮询 + 通用工具。
 * 布局对齐 ivec.ai 管理台（aic/ui/layout/user.html + header.html + vhtml-ui sidebar）：
 * 顶栏白底渐变 + 品牌区，浅色侧栏菜单（激活 = 主色 10% 底 + 主色文字）。
 * 每个页面 body 标 data-page，引入本模块即自动渲染骨架并启动状态轮询。
 */

// ---- 侧栏导航（多页：各自独立 HTML，刷新停留在当前页） ----

const ICONS = {
  settings: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 0 1 0 2.83 2 2 0 0 1-2.83 0l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-2 2 2 2 0 0 1-2-2v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 0 1-2.83 0 2 2 0 0 1 0-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1-2-2 2 2 0 0 1 2-2h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 0 1 0-2.83 2 2 0 0 1 2.83 0l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 2-2 2 2 0 0 1 2 2v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 0 1 2.83 0 2 2 0 0 1 0 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 2 2 2 2 0 0 1-2 2h-.09a1.65 1.65 0 0 0-1.51 1z"/></svg>',
  history: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><polyline points="12 6 12 12 16 14"/></svg>',
  files: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M22 19a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h5l2 3h9a2 2 0 0 1 2 2z"/></svg>',
  about: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><line x1="12" y1="16" x2="12" y2="12"/><line x1="12" y1="8" x2="12.01" y2="8"/></svg>',
};

const NAV = [
  { page: "settings", title: "设置", href: "options.html", icon: ICONS.settings },
  { page: "history", title: "执行历史", href: "history.html", icon: ICONS.history },
  { page: "files", title: "本地文件", href: "files.html", icon: ICONS.files },
  { page: "about", title: "关于", href: "about.html", icon: ICONS.about },
];

// renderShell 渲染顶栏 + 侧栏（页面提供 #topbar 与 #sidebar 挂载点）。
export function renderShell(active) {
  const topbar = document.getElementById("topbar");
  if (topbar) {
    topbar.innerHTML = `
      <div class="brand">
        <img src="../icons/logo.svg" alt="AIC" class="brand-mark">
        <div class="brand-copy">
          <div class="brand-title">AIC Browser</div>
          <div class="brand-subtitle">Browser Host</div>
        </div>
      </div>
      <div class="spacer"></div>
      <div class="conn-pill">
        <span id="conn-indicator" class="indicator disconnected"></span>
        <span id="conn-text">未连接</span>
      </div>`;
  }
  const el = document.getElementById("sidebar");
  if (el) {
    el.innerHTML = NAV.map(
      (n) => `<a class="menu-item${n.page === active ? " active" : ""}" href="${n.href}">
        <span class="menu-icon">${n.icon}</span><span class="menu-label">${n.title}</span>
      </a>`,
    ).join("");
  }
}

// ---- 连接状态轮询（顶栏指示灯） ----

export function updateConnectionStatus(connected) {
  const indicator = document.getElementById("conn-indicator");
  const text = document.getElementById("conn-text");
  if (!indicator || !text) return;
  indicator.className = "indicator " + (connected ? "connected" : "disconnected");
  text.textContent = connected ? "已连接" : "未连接";
}

export function startStatusPolling() {
  const check = () => {
    try {
      chrome.runtime.sendMessage({ type: "status" }, (resp) => {
        updateConnectionStatus(!chrome.runtime.lastError && resp?.connected === true);
      });
    } catch {
      updateConnectionStatus(false);
    }
  };
  check();
  setInterval(check, 5000);
}

// ---- 通用工具 ----

export function showStatus(el, msg, type) {
  el.textContent = msg;
  el.style.color = type === "error" ? "var(--color-danger)" : type === "success" ? "var(--color-success)" : "var(--text-color-tertiary)";
  setTimeout(() => { el.textContent = ""; }, 3000);
}

export function escapeHtml(s) {
  return String(s ?? "").replace(/[&<>"']/g, (c) => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
  }[c]));
}

export function truncate(s, n) {
  return s && s.length > n ? s.slice(0, n) + "…" : s;
}

export function fmtSize(n) {
  if (n === undefined || n === null) return "-";
  if (n < 1024) return `${n}B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)}K`;
  return `${(n / 1024 / 1024).toFixed(1)}M`;
}

export function cmpStr(a, b) {
  return a < b ? -1 : a > b ? 1 : 0;
}

// ---- 自动初始化（页面 body[data-page]） ----

renderShell(document.body.dataset.page || "");
startStatusPolling();
