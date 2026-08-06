/**
 * browser.js — browser 指令集的 host 端实现（Chrome 扩展，§5）
 *
 * 向 agent-browser CLI 基准靠拢（§5：物理端向 agent-browser 靠拢，而不是相反）：
 *   - 核心 13 action：open/click/close/download/eval/get/network/read/screenshot/
 *     snapshot/tab/wait/sleep，输入输出结构与 agent-browser 一致
 *   - eval 走 chrome.debugger CDP Runtime.evaluate（DevTools 语义：不受页面 CSP
 *     限制、可 await Promise、可访问页面 JS 变量；每次 attach/detach 短提示条）
 *   - agent-browser 独有子命令（type/fill/back/reload/upload 等）未实现 →
 *     统一 unknown action（help Unsupported 区标注，输入/导航用 eval 替代）
 *   - @ref 代次机制（§5.3）：每次 snapshot 递增代次号并写入元素定位属性
 *     data-aic-ref="{gen}:e{N}"；按当前 gen 解析，不命中即报统一 stale ref 错误
 *   - flags 全双横线长形式（§2.1 双层解析，sdk/argv.js）
 */

import { parseActionArgv } from "../sdk/argv.js";
import { loadSettings } from "../sdk/storage.js";

// ---- action 全集与 flag 表（§5.2/§2.1） ----

const CORE_ACTIONS = [
  "open", "click", "close", "download", "eval", "get", "network",
  "read", "screenshot", "snapshot", "tab", "wait", "sleep",
];
// 真实支持集（2026-08-06：EXT_ACTIONS 占位 save/upload/pipeline/var 已删除——
// var/pipeline/save 为旧设计废弃；upload 未实现，列入 help Unsupported 区）。
const ALL_ACTIONS = [...CORE_ACTIONS].sort();

// agent-browser CLI 有但本 host 未实现的子命令（help Unsupported 区展示，
// 调用即报 unknown action；type/fill 等输入类用 eval 注入 JS 替代）。
const UNSUPPORTED_ACTIONS = [
  "type", "fill", "press", "keyboard", "hover", "focus", "check", "uncheck",
  "select", "drag", "dblclick", "scroll", "scrollintoview", "pdf", "back",
  "forward", "reload", "is", "find", "mouse", "set", "cookies", "storage",
  "diff", "trace", "profiler", "record", "console", "errors", "highlight",
  "inspect", "clipboard", "batch", "auth", "session", "connect", "pushstate",
  "mcp", "skills", "upload",
];

const FLAG_SETS = {
  open: {},
  click: {},
  close: {},
  download: {},
  eval: { base64: "bool" },
  get: {},
  network: { filter: "value", type: "value", method: "value", status: "value", clear: "bool", limit: "value" },
  read: {},
  screenshot: { quality: "value", full: "bool" },
  snapshot: { interactive: "bool", urls: "bool", compact: "bool", depth: "value", selector: "value" },
  tab: {},
  wait: { url: "value", load: "value", fn: "value", text: "value", download: "value" },
  sleep: {},
};

const READ_MAX_BYTES = 100 * 1024; // §5.5：read 上限 100K 字节
const NETWORK_LIMIT_DEFAULT = 100; // §5.5：network 列表默认截断 100

// ---- @ref 代次注册表（§5.3）：tabId → 当前 snapshot 代次号 ----

const refGenByTab = new Map();

// ---- main entry ----
// 指令集 v2：作为 exec 虚拟指令接入，data = {action: "browser", argv: [subcommand, ...args]}。
// 子命令 = argv[0]，子命令参数 = argv.slice(1)（与 Go vcore/browser.Handle 语义一致）。

export async function browserHandler(ctx, toolData) {
  const argv = Array.isArray(toolData?.argv) ? toolData.argv.map(String) : [];
  const action = String(argv[0] || "").trim().toLowerCase();
  const rest = argv.slice(1);

  if (!action) {
    return err0(`browser: subcommand is required (supported: ${ALL_ACTIONS.join(", ")})`);
  }
  const flagSet = FLAG_SETS[action];
  if (!flagSet) {
    // 未实现子命令（agent-browser 独有）与未知子命令统一报 unknown action；
    // supported 列表只列真实支持集（2026-08-06 修复：旧版含 EXT 占位名误导 AI）。
    return err0(`browser: unknown action "${action}" (supported: ${ALL_ACTIONS.join(", ")}; unsupported: ${UNSUPPORTED_ACTIONS.join(", ")})`);
  }

  let pa;
  try {
    pa = parseActionArgv("browser", action, rest, flagSet);
  } catch (e) {
    return err0(e.message);
  }

  try {
    const result = await executeAction(action, pa, ctx);
    return result;
  } catch (e) {
    return err0(`browser ${action}: ${e.message}`);
  }
}

function err0(message) {
  return { state: "error", error: message, content: message };
}

async function executeAction(action, pa, ctx) {
  switch (action) {
    case "open": return await actOpen(pa);
    case "click": return await actClick(pa);
    case "close": return await actClose(pa);
    case "download": return await actDownload(pa);
    case "eval": return await actEval(pa);
    case "get": return await actGet(pa);
    case "network": return await actNetwork(pa);
    case "read": return await actRead(pa);
    case "screenshot": return await actScreenshot(pa, ctx);
    case "snapshot": return await actSnapshot(pa);
    case "tab": return await actTab(pa);
    case "wait": return await actWait(pa);
    case "sleep": return await actSleep(pa);
  }
  return err0(`browser: unknown action "${action}" (supported: ${ALL_ACTIONS.join(", ")})`);
}

// ---- tab helpers ----

async function getActiveTab() {
  const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
  if (!tab) throw new Error("no active tab");
  return tab;
}

// ---- AI 工作区（后台/隐私模式） ----
// 目标解析优先级：
//   incognito=true  → 独立无痕窗口内 AI 标签页（不共享 cookie/登录态，与用户主窗口完全隔离）
//   background=true → 普通窗口内 AI 标签页（共享登录态，创建 active:false 不抢焦点）
//   两者都关        → 当前激活标签页（协作模式，原行为）
// 状态持久化 chrome.storage.local（MV3 SW 休眠/重启后恢复；tab 被误关后自动重建）。
// 工作区模式所有操作不激活 tab、不改变窗口焦点——绝不与用户抢操作。

const WORKER_STORAGE_KEY = "aicWorker";

async function loadWorkerState() {
  const r = await chrome.storage.local.get(WORKER_STORAGE_KEY);
  return r[WORKER_STORAGE_KEY] || null;
}

async function saveWorkerState(state) {
  await chrome.storage.local.set({ [WORKER_STORAGE_KEY]: state || null });
}

// 当前是否 AI 工作区模式（后台 or 隐私）
async function workerMode() {
  const s = await loadSettings();
  return s.incognito || s.background !== false;
}

// 目标 tab 解析：工作区模式返回 AI 专属 tab，协作模式返回用户当前激活 tab
async function getTargetTab() {
  const s = await loadSettings();
  if (s.incognito) return ensureWorkerTab(true);
  if (s.background !== false) return ensureWorkerTab(false);
  return getActiveTab();
}

// 找非无痕 normal 窗口（普通窗口模式避免落在用户的无痕窗口）
async function findNormalWindow() {
  const wins = await chrome.windows.getAll({ windowTypes: ["normal"] });
  return wins.find((w) => !w.incognito) || wins[0] || null;
}

// 确保 AI 工作区 tab 存在（incognito=true 时在无痕窗口内），返回可用 tab
async function ensureWorkerTab(incognito) {
  const st = await loadWorkerState();
  if (st && st.winIncognito === incognito) {
    try {
      const tab = await chrome.tabs.get(st.tabId);
      const win = await chrome.windows.get(tab.windowId);
      if (win.incognito === incognito) return tab;
    } catch { /* tab/窗口已关闭，重建 */ }
  }

  let winId;
  if (incognito) {
    const wins = await chrome.windows.getAll({ windowTypes: ["normal"] });
    const existing = wins.find((w) => w.incognito);
    if (existing) {
      winId = existing.id;
    } else {
      let win;
      try {
        win = await chrome.windows.create({ incognito: true, focused: false });
      } catch (e) {
        throw new Error(`incognito window create failed: enable the extension in incognito (chrome://extensions → AIC Browser → Allow in Incognito): ${e?.message || e}`);
      }
      winId = win.id;
    }
  } else {
    const win = await findNormalWindow();
    if (!win) throw new Error("no browser window available");
    winId = win.id;
  }

  // 无痕窗口创建时自带一个 about:blank tab，直接复用；否则显式创建（不激活不抢焦点）
  let tab;
  if (incognito) {
    const tabs = await chrome.tabs.query({ windowId: winId });
    tab = tabs[0] || (await chrome.tabs.create({ windowId: winId, active: false }));
  } else {
    tab = await chrome.tabs.create({ windowId: winId, url: "about:blank", active: false });
  }
  await saveWorkerState({ winId, tabId: tab.id, winIncognito: incognito });
  return tab;
}

// 更新工作区当前 tab（tab new/N/close 后），不激活不抢焦点
async function setWorkerTab(tabId) {
  const st = await loadWorkerState();
  await saveWorkerState({ ...(st || {}), tabId });
}

async function execInTab(tabId, func, args) {
  const results = await chrome.scripting.executeScript({
    target: { tabId },
    func,
    args: args || [],
  });
  if (!results || results.length === 0) throw new Error("script execution failed");
  return results[0].result;
}

// 元素定位（§5.3）：@e<N> 按当前 gen 查找 data-aic-ref 属性；
// 不命中即 stale（页面变化后必须重新 snapshot）。CSS 选择器原样支持。
function staleRefError(action, ref) {
  return `browser ${action}: stale ref ${ref} (page changed since last snapshot; run snapshot again)`;
}

// resolveElementInPage 注入页面的元素查找器：返回 {found} | {stale} | {missing}
function makeResolveArgs(tabId, sel) {
  if (sel.startsWith("@e")) {
    const gen = refGenByTab.get(tabId) || 0;
    return { ref: `${gen}:${sel.slice(1)}` }; // "gen:eN"
  }
  return { css: sel };
}

// ---- open ----

async function actOpen(pa) {
  const url = pa.positional[0];
  if (!url) throw new Error("url is required");
  if (!url.startsWith("http://") && !url.startsWith("https://")) {
    throw new Error("only http(s) urls are supported");
  }
  const tab = await getTargetTab();
  await chrome.tabs.update(tab.id, { url });
  await waitTabLoad(tab.id, 15000);
  const updated = await chrome.tabs.get(tab.id);
  return {
    content: `✓ ${updated.title || updated.url}\n  ${updated.url}`,
    attrs: { action: "open" },
  };
}

function waitTabLoad(tabId, ms) {
  return new Promise((resolve) => {
    const timeout = setTimeout(resolve, ms);
    const listener = (updatedTabId, info) => {
      if (updatedTabId === tabId && info.status === "complete") {
        clearTimeout(timeout);
        chrome.tabs.onUpdated.removeListener(listener);
        setTimeout(resolve, 300);
      }
    };
    chrome.tabs.onUpdated.addListener(listener);
  });
}

// ---- click ----

async function actClick(pa) {
  const sel = pa.positional[0];
  if (!sel) throw new Error("selector or @ref is required");
  const tab = await getTargetTab();
  const loc = makeResolveArgs(tab.id, sel);
  const result = await execInTab(tab.id, (_loc) => {
    let el = null;
    if (_loc.ref) {
      el = document.querySelector(`[data-aic-ref="${_loc.ref}"]`);
      if (!el) return { stale: true };
    } else {
      el = document.querySelector(_loc.css);
      if (!el) return { missing: true };
    }
    el.scrollIntoView({ block: "center", behavior: "instant" });
    el.click();
    return { ok: true };
  }, [loc]);

  if (result?.stale) throw new Error(staleRefError("click", sel).replace(/^browser click: /, ""));
  if (result?.missing) throw new Error(`element not found: ${sel}`);
  return { content: `✓ Clicked ${sel}`, attrs: { action: "click" } };
}

// ---- close ----

async function actClose(pa) {
  if (!(await workerMode())) {
    const tabs = await chrome.tabs.query({ currentWindow: true });
    if (tabs.length <= 1) {
      // §5.5：host 由浏览器自身行为决定，拒绝关闭最后 tab 非错误
      return { content: "Cannot close last tab", attrs: { action: "close", closed: "false" } };
    }
    const tab = await getActiveTab();
    await chrome.tabs.remove(tab.id);
    return { content: "✓ Browser closed", attrs: { action: "close", closed: "true" } };
  }
  // 工作区模式：关闭 AI 工作区当前 tab（不碰用户页面）
  const wtab = await getTargetTab();
  const tabs = await chrome.tabs.query({ windowId: wtab.windowId });
  if (tabs.length <= 1) {
    return { content: "Cannot close last AI tab", attrs: { action: "close", closed: "false" } };
  }
  await chrome.tabs.remove(wtab.id);
  // 清空工作区状态：下一条指令重建干净的 AI tab（避免目标漂移到用户 tab）
  await saveWorkerState(null);
  return { content: "✓ AI tab closed", attrs: { action: "close", closed: "true" } };
}

// ---- download ----

async function actDownload(pa) {
  const sel = pa.positional[0];
  const filename = pa.positional[1];
  if (!sel || !filename) throw new Error("download requires selector and path");

  const tab = await getTargetTab();
  const loc = makeResolveArgs(tab.id, sel);

  // 监听 chrome.downloads 捕获下载
  const downloadPromise = new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      chrome.downloads.onCreated.removeListener(listener);
      reject(new Error("timeout waiting for download"));
    }, 30000);
    const listener = (item) => {
      clearTimeout(timer);
      chrome.downloads.onCreated.removeListener(listener);
      resolve(item);
    };
    chrome.downloads.onCreated.addListener(listener);
  });

  const clickResult = await execInTab(tab.id, (_loc) => {
    let el = null;
    if (_loc.ref) {
      el = document.querySelector(`[data-aic-ref="${_loc.ref}"]`);
      if (!el) return { stale: true };
    } else {
      el = document.querySelector(_loc.css);
      if (!el) return { missing: true };
    }
    el.click();
    return { ok: true };
  }, [loc]);

  if (clickResult?.stale) throw new Error(staleRefError("download", sel).replace(/^browser download: /, ""));
  if (clickResult?.missing) throw new Error(`element not found: ${sel}`);

  const item = await downloadPromise;
  // §5.6：host 存浏览器下载目录，attrs path 为本地路径
  return {
    content: `✓ Downloaded: ${item.filename || filename}`,
    attrs: { action: "download", path: item.filename || filename },
  };
}

// ---- eval（§5.x：chrome.debugger CDP Runtime.evaluate，DevTools 语义）----
// 完全不受页面 CSP script-src/unsafe-eval 限制（等价 DevTools 控制台执行）；
// 可访问页面 JS 变量（window.xxx/框架内部状态），awaitPromise 等待 Promise。
// 每次 eval attach/detach（Chrome 短暂显示调试提示条）；finally 保证 detach；
// 超时保护防页面 Promise 挂死。

const CDP_EVAL_TIMEOUT_MS = 30000; // awaitPromise 上限（页面 Promise 挂死防呆）
const CDP_PROTOCOL = "1.3";

async function cdpEval(tabId, js) {
  let attachErr;
  try {
    attachErr = await chrome.debugger.attach({ tabId }, CDP_PROTOCOL);
  } catch (e) {
    throw new Error(`debugger attach failed: ${e?.message || e}`);
  }
  if (attachErr) {
    // attach 返回错误串（如该 tab 已被 DevTools/其他调试器占用）
    throw new Error(`debugger attach failed: ${attachErr}`);
  }
  try {
    const res = await withTimeout(
      chrome.debugger.sendCommand({ tabId }, "Runtime.evaluate", {
        expression: js,
        awaitPromise: true,
        returnByValue: true,
      }),
      CDP_EVAL_TIMEOUT_MS,
      `eval timeout after ${CDP_EVAL_TIMEOUT_MS / 1000}s (awaitPromise)`,
    );
    if (res?.exceptionDetails) {
      const d = res.exceptionDetails;
      const desc = d.exception?.description || d.text || "exception";
      throw new Error(desc);
    }
    return res?.result?.value;
  } finally {
    try {
      await chrome.debugger.detach({ tabId });
    } catch (_) {
      /* tab 已关闭/已 detach：忽略 */
    }
  }
}

// CDP 截图（2026-08-06：captureVisibleTab 只能截当前激活 tab，工作区 tab 不激活——
// 统一走 Page.captureScreenshot，attach 不要求 tab 活跃，与 eval 同通道）。
async function cdpScreenshot(tabId, quality) {
  let attachErr;
  try {
    attachErr = await chrome.debugger.attach({ tabId }, CDP_PROTOCOL);
  } catch (e) {
    throw new Error(`debugger attach failed: ${e?.message || e}`);
  }
  if (attachErr) {
    throw new Error(`debugger attach failed: ${attachErr}`);
  }
  try {
    const res = await withTimeout(
      chrome.debugger.sendCommand({ tabId }, "Page.captureScreenshot", {
        format: "jpeg",
        quality,
      }),
      CDP_EVAL_TIMEOUT_MS,
      `screenshot timeout after ${CDP_EVAL_TIMEOUT_MS / 1000}s`,
    );
    if (!res?.data) throw new Error("Page.captureScreenshot returned no data");
    const bin = atob(res.data);
    const bytes = new Uint8Array(bin.length);
    for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
    // MV3 SW 没有 URL.createObjectURL（实测 "is not a function"）——直接返回 Blob 落盘
    return new Blob([bytes], { type: "image/jpeg" });
  } finally {
    try {
      await chrome.debugger.detach({ tabId });
    } catch (_) {
      /* tab 已关闭/已 detach：忽略 */
    }
  }
}

function withTimeout(p, ms, msg) {
  return new Promise((resolve, reject) => {
    const t = setTimeout(() => reject(new Error(msg)), ms);
    p.then(
      (v) => {
        clearTimeout(t);
        resolve(v);
      },
      (e) => {
        clearTimeout(t);
        reject(e);
      },
    );
  });
}

async function actEval(pa) {
  let js = pa.positional[0];
  if (pa.bools["base64"]) {
    js = decodeBase64(js || "");
  }
  if (!js) throw new Error("script is required");
  const tab = await getTargetTab();
  const value = await cdpEval(tab.id, js);
  return { content: formatEvalValue(value), attrs: { action: "eval" } };
}

function formatEvalValue(value) {
  if (value === undefined) return "undefined";
  if (typeof value === "string") return value;
  try {
    return JSON.stringify(value);
  } catch {
    return String(value);
  }
}

function decodeBase64(s) {
  try {
    return decodeURIComponent(escape(atob(s)));
  } catch {
    return atob(s);
  }
}

// ---- get ----

async function actGet(pa) {
  const what = pa.positional[0];
  if (!what) throw new Error("get requires a sub-command: text/html/title/url/value/attr/count/box/styles");
  const tab = await getTargetTab();

  switch (what) {
    case "title":
      return { content: tab.title || "", attrs: { action: "get" } };
    case "url":
      return { content: tab.url || "", attrs: { action: "get" } };
  }

  // 需要选择器的子命令：get text/html [sel?]、get value/box/styles <sel>、get attr <sel> <name>、get count <sel>
  let sel = null, attrName = null;
  if (what === "attr") {
    sel = pa.positional[1];
    attrName = pa.positional[2];
    if (!sel || !attrName) throw new Error("get attr requires selector and attribute name");
  } else if (["value", "box", "styles", "count"].includes(what)) {
    sel = pa.positional[1];
    if (!sel) throw new Error(`get ${what} requires a selector`);
  } else if (["text", "html"].includes(what)) {
    sel = pa.positional[1] || null; // 省略=body/documentElement
  } else {
    throw new Error(`unknown get sub-command "${what}" (supported: text, html, title, url, value, attr, count, box, styles)`);
  }

  const loc = sel ? makeResolveArgs(tab.id, sel) : null;
  const result = await execInTab(tab.id, (_what, _loc, _attrName) => {
    let el = null;
    if (_loc) {
      if (_loc.ref) {
        el = document.querySelector(`[data-aic-ref="${_loc.ref}"]`);
        if (!el) return { stale: true };
      } else {
        el = document.querySelector(_loc.css);
        if (!el && _what !== "count") return { missing: true };
      }
    }
    switch (_what) {
      case "text":
        return { text: (el || document.body).innerText };
      case "html":
        return { text: (el || document.documentElement).outerHTML };
      case "value":
        return { text: el.value ?? "" };
      case "attr":
        return { text: el.getAttribute(_attrName) ?? "" };
      case "count":
        return { text: String(el ? 1 : document.querySelectorAll(_loc.css).length) };
      case "box": {
        const r = el.getBoundingClientRect();
        return { text: JSON.stringify({ x: r.x, y: r.y, width: r.width, height: r.height }) };
      }
      case "styles": {
        const cs = window.getComputedStyle(el);
        const out = {};
        for (const name of cs) out[name] = cs.getPropertyValue(name);
        return { text: JSON.stringify(out) };
      }
    }
    return { error: `unknown get sub-command: ${_what}` };
  }, [what, loc, attrName]);

  if (result?.stale) throw new Error(staleRefError("get", sel).replace(/^browser get: /, ""));
  if (result?.missing) throw new Error(`element not found: ${sel}`);
  if (result?.error) throw new Error(result.error);
  return { content: result?.text ?? "", attrs: { action: "get" } };
}

// ---- network ----

async function actNetwork(pa) {
  const tab = await getTargetTab();

  if (pa.bools["clear"]) {
    await execInTab(tab.id, () => { window.__aic_network_logs = []; }, []);
    return { content: "✓ Network log cleared", attrs: { action: "network" } };
  }

  const reqId = pa.positional[0] || null;
  // "requests" 是 list 模式的别名（help 语法），不能当作请求 id
  if (reqId && reqId !== "requests") {
    const result = await execInTab(tab.id, (_reqId) => {
      const logs = window.__aic_network_logs || [];
      const req = logs.find((r) => r.id === _reqId);
      if (!req) return { error: `request not found: ${_reqId}` };
      return { text: JSON.stringify(req, null, 2) };
    }, [reqId]);
    if (result?.error) throw new Error(result.error);
    return { content: result.text, attrs: { action: "network" } };
  }

  let limit = NETWORK_LIMIT_DEFAULT;
  if (pa.flags["limit"]) {
    const n = parseInt(pa.flags["limit"], 10);
    if (n > 0) limit = n;
  }
  const result = await execInTab(tab.id, (_flags) => {
    let logs = window.__aic_network_logs || [];
    if (_flags.filter) {
      const f = _flags.filter.toLowerCase();
      logs = logs.filter((r) => r.url.toLowerCase().includes(f));
    }
    if (_flags.type) {
      const types = _flags.type.split(",");
      logs = logs.filter((r) => types.includes(r.type));
    }
    if (_flags.method) {
      logs = logs.filter((r) => r.method === _flags.method.toUpperCase());
    }
    if (_flags.status) {
      logs = logs.filter((r) => r.status === parseInt(_flags.status, 10));
    }
    // §5.5 列表格式：[{id}] {method} {url} ({type}) {status}
    return { lines: logs.map((r) => `[${r.id}] ${r.method} ${r.url} (${r.type}) ${r.status}`) };
  }, [pa.flags]);

  const lines = result?.lines || [];
  const truncated = lines.length > limit;
  const shown = truncated ? lines.slice(0, limit) : lines;
  return {
    content: shown.length ? shown.join("\n") : "(no network requests)",
    attrs: { action: "network", rows: String(shown.length), truncated: String(truncated) },
  };
}

// ---- read ----

async function actRead(pa) {
  const url = pa.positional[0] || null;
  let text;
  if (url) {
    const resp = await fetch(url, { headers: { Accept: "text/html" } });
    const html = await resp.text();
    text = extractReadableText(html);
  } else {
    const tab = await getTargetTab();
    const result = await execInTab(tab.id, () => document.documentElement.outerHTML, []);
    text = extractReadableText(result || "");
  }
  // §5.5：上限 100K 字节，超出尾部追加 "\n... (truncated)"
  let truncated = false;
  const encoder = new TextEncoder();
  if (encoder.encode(text).length > READ_MAX_BYTES) {
    // rune 边界收刀
    let cut = READ_MAX_BYTES;
    while (cut > 0 && encoder.encode(text.slice(0, cut)).length > READ_MAX_BYTES) cut--;
    text = text.slice(0, cut) + "\n... (truncated)";
    truncated = true;
  }
  return { content: text, attrs: { action: "read", truncated: String(truncated) } };
}

function extractReadableText(html) {
  // MV3 SW 无 DOMParser（实测 "DOMParser is not defined"）——正则剥离：
  // script/style/noscript → 块级闭合标签与 <br> 插换行 → 剥标签 → 实体解码 → 压空白。
  return html
    .replace(/<script[\s\S]*?<\/script>/gi, " ")
    .replace(/<style[\s\S]*?<\/style>/gi, " ")
    .replace(/<noscript[\s\S]*?<\/noscript>/gi, " ")
    .replace(/<\/(div|p|h[1-6]|li|tr|td|th|section|article|header|footer|main|nav|ul|ol|table|blockquote|pre|form|fieldset)>/gi, "\n")
    .replace(/<br\s*\/?>/gi, "\n")
    .replace(/<[^>]+>/g, " ")
    .replace(/&nbsp;/g, " ")
    .replace(/&amp;/g, "&")
    .replace(/&lt;/g, "<")
    .replace(/&gt;/g, ">")
    .replace(/&quot;/g, '"')
    .replace(/&#0*39;/g, "'")
    .replace(/[ \t]+\n/g, "\n")
    .replace(/\n{3,}/g, "\n\n")
    .trim();
}

// ---- screenshot ----

async function actScreenshot(pa, ctx) {
  const quality = Math.min(parseInt(pa.flags["quality"] || "80", 10), 100);
  const tab = await getTargetTab();
  // §2.2：browser 不返回图片数据（仅 fs.read 能把图片带进消息）。
  // 截图落本 host 的 fs（/screenshot/，扩展 IndexedDB Blob 存储，本地单根），
  // agent 需要读图时用 fs.read（1host=本 host_id）按 attrs.path 读取。
  if (!ctx?.fs) throw new Error("fs backend not available on this host");
  const blob = await cdpScreenshot(tab.id, quality);
  const name = `screenshot-${new Date().toISOString().replace(/[:.]/g, "-")}.jpg`;
  const out = await ctx.fs.put(`/screenshot/${name}`, blob);
  return {
    content: `✓ Screenshot saved to ${out.path} (${out.bytes} bytes; read it with fs.read on this host)`,
    attrs: { action: "screenshot", path: out.path },
  };
}

// ---- snapshot（§5.4 树形格式 + §5.3 @ref 代次） ----

async function actSnapshot(pa) {
  const tab = await getTargetTab();
  const gen = (refGenByTab.get(tab.id) || 0) + 1;
  refGenByTab.set(tab.id, gen);

  const opts = {
    interactive: pa.bools["interactive"],
    urls: pa.bools["urls"],
    compact: pa.bools["compact"],
    depth: pa.flags["depth"] ? parseInt(pa.flags["depth"], 10) : 0,
    selector: pa.flags["selector"] || null,
    gen,
  };

  const result = await execInTab(tab.id, (_opts) => {
    let uidCounter = 0;
    const lines = [];

    const SKIP_TAGS = ["script", "style", "noscript", "meta", "link", "br", "hr"];

    function roleOf(el) {
      const role = el.getAttribute("role");
      if (role) return role;
      const tag = el.tagName.toLowerCase();
      const type = (el.getAttribute("type") || "").toLowerCase();
      const map = {
        a: "link", button: "button",
        input: type === "checkbox" ? "checkbox" : type === "radio" ? "radio"
          : ["submit", "button", "reset"].includes(type) ? "button" : "textbox",
        select: "combobox", textarea: "textbox",
        img: "img", h1: "heading", h2: "heading", h3: "heading",
        h4: "heading", h5: "heading", h6: "heading",
        nav: "navigation", main: "main", form: "form", table: "table",
        li: "listitem", ul: "list", ol: "list",
        section: "section", article: "article", aside: "complementary",
        header: "banner", footer: "contentinfo", p: "paragraph",
      };
      return map[tag] || tag;
    }

    function accessibleName(el) {
      const label = el.getAttribute("aria-label") || el.getAttribute("title")
        || el.getAttribute("placeholder") || el.getAttribute("alt") || "";
      if (label) return label;
      if (["a", "button"].includes(el.tagName.toLowerCase())) {
        return el.textContent.trim().slice(0, 80);
      }
      return "";
    }

    function isInteractive(el) {
      const tag = el.tagName.toLowerCase();
      const role = (el.getAttribute("role") || "").toLowerCase();
      if (["button", "a", "input", "select", "textarea", "option"].includes(tag)) return true;
      if (["button", "link", "checkbox", "radio", "textbox", "combobox", "menuitem", "tab", "switch", "option"].includes(role)) return true;
      if (el.hasAttribute("onclick") || el.getAttribute("tabindex") === "0") return true;
      return false;
    }

    function attrList(el, ref) {
      const parts = [];
      const tag = el.tagName.toLowerCase();
      const role = roleOf(el);
      if (role === "heading") {
        const level = parseInt(tag[1], 10);
        if (level >= 1 && level <= 6) parts.push(`level=${level}`);
      }
      if (_opts.urls && tag === "a" && el.hasAttribute("href")) {
        parts.push(`href="${el.getAttribute("href")}"`);
      }
      if (el.hasAttribute("checked")) parts.push("checked");
      if (el.hasAttribute("disabled")) parts.push("disabled");
      if (ref) parts.push(`ref=${ref}`);
      return parts;
    }

    function visible(el) {
      const style = window.getComputedStyle(el);
      return style.display !== "none" && style.visibility !== "hidden";
    }

    function hasLocatable(el) {
      return isInteractive(el) || ["heading", "link", "button", "textbox", "checkbox", "radio", "combobox"].includes(roleOf(el));
    }

    function walk(el, d, indent) {
      if (el.nodeType !== 1) return;
      if (_opts.depth > 0 && d > _opts.depth) return;
      const tag = el.tagName.toLowerCase();
      if (SKIP_TAGS.includes(tag) || !visible(el)) return;

      if (_opts.interactive && !isInteractive(el)) {
        for (const child of el.children) walk(child, d, indent);
        return;
      }
      if (_opts.compact && !isInteractive(el) && el.children.length > 0) {
        for (const child of el.children) walk(child, d, indent);
        return;
      }

      // §5.4：- {role} ["{name}"] [{属性}, ref=e{N}]，缩进两个空格/层
      const role = roleOf(el);
      const name = accessibleName(el);
      let ref = "";
      if (hasLocatable(el)) {
        uidCounter++;
        ref = `e${uidCounter}`;
        // §5.3：写入元素定位属性 data-aic-ref="{gen}:e{N}"
        el.setAttribute("data-aic-ref", `${_opts.gen}:${ref}`);
      }
      const attrs = attrList(el, ref);
      let line = "- " + role;
      if (name) line += ` "${name.replace(/"/g, '\\"')}"`;
      if (attrs.length) line += ` [${attrs.join(", ")}]`;
      lines.push("  ".repeat(indent) + line);

      // 纯文本节点以 - StaticText "..." 表示
      for (const node of el.childNodes) {
        if (node.nodeType === 3) {
          const t = node.textContent.trim();
          if (t && el.children.length === 0) {
            lines.push("  ".repeat(indent + 1) + `- StaticText "${t.slice(0, 200).replace(/"/g, '\\"')}"`);
          }
        }
      }
      for (const child of el.children) walk(child, d + 1, indent + 1);
    }

    const root = _opts.selector ? document.querySelector(_opts.selector) : document.body;
    if (!root) return { error: `selector not found: ${_opts.selector}` };
    walk(root, 0, 0);
    return { lines, count: uidCounter };
  }, [opts]);

  if (result?.error) throw new Error(result.error);

  const updated = await chrome.tabs.get(tab.id);
  // §5.4 首两行：✓ {页面标题} + 缩进的当前 URL
  const header = `✓ ${updated.title || updated.url}\n  ${updated.url}`;
  const body = (result?.lines || []).join("\n");
  return {
    content: body ? header + "\n" + body : header,
    attrs: { action: "snapshot", rows: String(result?.count || 0) },
  };
}

// ---- tab（§5.5：先匹配关键字，再按序号解析，序号 1 基） ----

async function actTab(pa) {
  const sub = pa.positional[0] || "list";

  // 协作模式：现状（操作当前窗口标签页，创建/切换会激活抢焦点）
  if (!(await workerMode())) {
    return actTabCoop(sub, pa);
  }

  // 工作区模式：tab 系列限定在 AI 工作区窗口内；切换只改 current 不激活 tab
  // （executeScript/CDP 不要求 tab 激活），绝不抢焦点。
  const wtab = await getTargetTab();
  const winId = wtab.windowId;

  if (sub === "new") {
    const tab = await chrome.tabs.create({ windowId: winId, active: false });
    await setWorkerTab(tab.id);
    return { content: `✓ Opened AI tab`, attrs: { action: "tab" } };
  }
  if (sub === "list") {
    const tabs = await chrome.tabs.query({ windowId: winId });
    const lines = tabs.map((t, i) => {
      let title = t.title || "";
      if (title.length > 80) title = title.slice(0, 77) + "...";
      const prefix = t.id === wtab.id ? "→ " : "";
      return `${prefix}${i + 1}\t${title}\t${t.url}`;
    });
    return {
      content: lines.join("\n"),
      attrs: { action: "tab", rows: String(tabs.length) },
    };
  }
  if (sub === "close") {
    const tabs = await chrome.tabs.query({ windowId: winId });
    if (tabs.length <= 1) {
      return { content: "Cannot close last AI tab", attrs: { action: "tab", closed: "false" } };
    }
    const nArg = pa.positional[1];
    let targetId;
    if (nArg !== undefined) {
      const n = parseInt(nArg, 10);
      if (isNaN(n) || n < 1 || n > tabs.length) {
        throw new Error(`tab index ${nArg} out of range (1-${tabs.length})`);
      }
      targetId = tabs[n - 1].id;
    } else {
      targetId = wtab.id;
    }
    await chrome.tabs.remove(targetId);
    // 清空工作区状态：下一条指令重建干净的 AI tab（避免目标漂移到用户 tab）
    await saveWorkerState(null);
    return { content: "✓ Closed AI tab", attrs: { action: "tab", closed: "true" } };
  }
  const n = parseInt(sub, 10);
  if (!isNaN(n)) {
    const tabs = await chrome.tabs.query({ windowId: winId });
    if (n < 1 || n > tabs.length) {
      throw new Error(`tab index ${n} out of range (1-${tabs.length})`);
    }
    await setWorkerTab(tabs[n - 1].id);
    return { content: `✓ Switched to AI tab ${n}`, attrs: { action: "tab" } };
  }
  throw new Error(`unknown tab sub-command "${sub}" (supported: new, list, close, <N>)`);
}

// 协作模式 tab 管理（现状：当前窗口，创建/切换激活抢焦点）
async function actTabCoop(sub, pa) {
  if (sub === "new") {
    await chrome.tabs.create({ active: true });
    const tabs = await chrome.tabs.query({ currentWindow: true });
    return { content: `✓ Opened tab ${tabs.length}`, attrs: { action: "tab" } };
  }
  if (sub === "list") {
    const tabs = await chrome.tabs.query({ currentWindow: true });
    const lines = tabs.map((t, i) => {
      let title = t.title || "";
      if (title.length > 80) title = title.slice(0, 77) + "...";
      const prefix = t.active ? "→ " : "";
      return `${prefix}${i + 1}\t${title}\t${t.url}`;
    });
    return {
      content: lines.join("\n"),
      attrs: { action: "tab", rows: String(tabs.length) },
    };
  }
  if (sub === "close") {
    const nArg = pa.positional[1];
    const tabs = await chrome.tabs.query({ currentWindow: true });
    if (tabs.length <= 1) {
      return { content: "Cannot close last tab", attrs: { action: "tab", closed: "false" } };
    }
    if (nArg !== undefined) {
      const n = parseInt(nArg, 10);
      if (isNaN(n) || n < 1 || n > tabs.length) {
        throw new Error(`tab index ${nArg} out of range (1-${tabs.length})`);
      }
      await chrome.tabs.remove(tabs[n - 1].id);
    } else {
      const active = await getActiveTab();
      await chrome.tabs.remove(active.id);
    }
    return { content: "✓ Closed tab", attrs: { action: "tab", closed: "true" } };
  }
  // 序号切换（1 基）
  const n = parseInt(sub, 10);
  if (!isNaN(n)) {
    const tabs = await chrome.tabs.query({ currentWindow: true });
    if (n < 1 || n > tabs.length) {
      throw new Error(`tab index ${n} out of range (1-${tabs.length})`);
    }
    await chrome.tabs.update(tabs[n - 1].id, { active: true });
    return { content: `✓ Switched to tab ${n}`, attrs: { action: "tab" } };
  }
  throw new Error(`unknown tab sub-command "${sub}" (supported: new, list, close, <N>)`);
}

// ---- wait（§5.5） ----

const WAIT_DEFAULT_MS = 30000;

async function actWait(pa) {
  const tab = await getTargetTab();
  const first = pa.positional[0];

  if (pa.flags["url"]) {
    const glob = pa.flags["url"];
    return await waitCondition(`url ${glob}`, WAIT_DEFAULT_MS, async () => {
      const t = await chrome.tabs.get(tab.id);
      return globMatchText(glob, t.url || "");
    });
  }
  if (pa.flags["load"]) {
    const cond = pa.flags["load"];
    if (cond === "load" || cond === "domcontentloaded") {
      await waitTabLoad(tab.id, WAIT_DEFAULT_MS);
      return { content: `✓ wait condition met: load ${cond}`, attrs: { action: "wait" } };
    }
    // networkidle：500ms 无新请求
    return await waitCondition(`load ${cond}`, WAIT_DEFAULT_MS, async () => {
      const quiet = await execInTab(tab.id, () => {
        const logs = window.__aic_network_logs || [];
        const now = Date.now();
        return logs.filter((r) => !r.end && now - r.start < 500).length === 0;
      }, []);
      return quiet;
    });
  }
  if (pa.flags["fn"]) {
    // --fn 是任意 JS 表达式：仍走 new Function（页面 CSP 限制时返回 false 超时，
    // 与 agent-browser 同语义；建议用 wait <selector> 替代）
    const js = pa.flags["fn"];
    const ok = await waitPoll(tab.id, WAIT_DEFAULT_MS, (_body) => {
      try { return new Function(_body)(); } catch { return false; }
    }, [`return !!(${js})`]);
    if (!ok) throw new Error(`timeout after ${WAIT_DEFAULT_MS}ms waiting for fn ${js}`);
    return { content: `✓ wait condition met: fn ${js}`, attrs: { action: "wait" } };
  }
  if (pa.flags["text"]) {
    const text = pa.flags["text"];
    const ok = await waitPoll(tab.id, WAIT_DEFAULT_MS, (_text) => {
      return !!(document.body && document.body.innerText.includes(_text));
    }, [text]);
    if (!ok) throw new Error(`timeout after ${WAIT_DEFAULT_MS}ms waiting for text ${text}`);
    return { content: `✓ wait condition met: text ${text}`, attrs: { action: "wait" } };
  }
  if (pa.flags["download"]) {
    const file = pa.flags["download"];
    const found = await waitDownload(file, WAIT_DEFAULT_MS);
    if (!found) throw new Error(`timeout after ${WAIT_DEFAULT_MS}ms waiting for download ${file}`);
    return { content: `✓ wait condition met: download ${file}`, attrs: { action: "wait" } };
  }
  if (!first) throw new Error("wait requires a condition (selector, ms, or --url/--load/--fn/--text/--download)");
  if (/^\d+$/.test(first)) {
    await sleepMs(parseInt(first, 10));
    return { content: `✓ wait condition met: ${first}ms`, attrs: { action: "wait" } };
  }
  // selector 等待（结构化参数注入，避开 CSP new Function 限制）
  const loc = makeResolveArgs(tab.id, first);
  const ok = await waitPoll(tab.id, WAIT_DEFAULT_MS, (_loc) => {
    if (_loc.ref) return !!document.querySelector(`[data-aic-ref="${_loc.ref}"]`);
    return !!document.querySelector(_loc.css);
  }, [loc]);
  if (!ok) throw new Error(`timeout after ${WAIT_DEFAULT_MS}ms waiting for ${first}`);
  return { content: `✓ wait condition met: ${first}`, attrs: { action: "wait" } };
}

async function waitCondition(desc, ms, checkFn) {
  const start = Date.now();
  while (Date.now() - start < ms) {
    try {
      if (await checkFn()) {
        return { content: `✓ wait condition met: ${desc}`, attrs: { action: "wait" } };
      }
    } catch { /* tab 切换等瞬态错误忽略 */ }
    await sleepMs(200);
  }
  throw new Error(`timeout after ${ms}ms waiting for ${desc}`);
}

// waitPoll 轮询执行注入函数（2026-08-06 修复：原实现注入 new Function 拼 JS，
// 页面 CSP 无 unsafe-eval 时抛 EvalError → 永远 false → wait <selector> 必超时。
// 改为结构化参数直传（execInTab 的 func 是真实函数体，不受 CSP 动态执行限制）。
async function waitPoll(tabId, ms, func, args) {
  const start = Date.now();
  while (Date.now() - start < ms) {
    try {
      const r = await execInTab(tabId, func, args || []);
      if (r) return true;
    } catch { /* ignore */ }
    await sleepMs(200);
  }
  return false;
}

async function waitDownload(filename, ms) {
  const start = Date.now();
  while (Date.now() - start < ms) {
    const items = await chrome.downloads.search({
      filenameRegex: filename.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"),
      state: "complete",
    });
    if (items.length > 0) return true;
    await sleepMs(300);
  }
  return false;
}

function globMatchText(glob, s) {
  const re = new RegExp("^" + glob.replace(/[.+^${}()|[\]\\]/g, "\\$&").replace(/\*/g, ".*").replace(/\?/g, ".") + "$");
  return re.test(s);
}

// ---- sleep ----

async function actSleep(pa) {
  const duration = pa.positional[0];
  if (!duration) throw new Error("duration is required (e.g. 1s, 500ms)");
  const ms = parseDuration(duration);
  if (ms === null) throw new Error(`invalid duration "${duration}" (e.g. 1s, 500ms)`);
  await sleepMs(ms);
  return { content: `✓ slept for ${duration}`, attrs: { action: "sleep" } };
}

function parseDuration(s) {
  const m = /^(\d+(?:\.\d+)?)(ms|s|m)?$/.exec(s);
  if (!m) return null;
  const n = parseFloat(m[1]);
  switch (m[2] || "ms") {
    case "ms": return n;
    case "s": return n * 1000;
    case "m": return n * 60000;
  }
  return null;
}

function sleepMs(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
