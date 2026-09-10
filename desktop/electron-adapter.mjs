/**
 * electron-adapter.mjs — browser core 的 desktop（Electron）宿主适配器 + 原生内容池
 *（OS 原生窗口内容模型，2026-09-10 P0；设计唯一源 = aic/docs/os_native_windows.md）
 *
 * 与 Chrome 插件适配器的映射关系（core.js adapter 契约）：
 *   tabs     → 主窗口内的 WebContentsView（AI 工作区标签页，webContents.id 即 tabId）
 *   windows  → 主窗口本身（BaseWindow）：desktop 无用户窗口概念，AI 工作区标签
 *              经桥协议贴进平台页 OS 的 Browser 窗口内容区
 *   evalIn   → webContents.executeJavaScript（func.toString() + JSON 参数序列化）
 *   cdp      → webContents.debugger（常驻 attach；网络拦截器经
 *              Page.addScriptToEvaluateOnNewDocument 注入，与插件 content script 同源）
 *   downloads→ partition session 的 will-download（产物落会话目录 .browser/）
 *   store    → 主进程内存 Map（Electron 主进程无休眠，工作区状态无需持久化）
 *   settings → 固定 { background: true }（desktop 不提供协作模式：AI 一律在
 *              AI 工作区标签页作业；平台标签页操作有自指风险）
 *
 * 原生内容池（docs §4 硬约束）：
 *   - 平台页（OS Browser 窗口占位元素）是 rect/可见性的唯一驱动源：
 *     tabControl.applyLayout({rect, visible})；rect = 占位元素 getBoundingClientRect
 *     （CSS px = DIP，contentView 相对——platformView 恒满窗且位于 (0,0)，zoom=1）。
 *   - 隐藏 = z 序回落 platformView 之下（遮挡隐藏：被遮挡的 WebContentsView 保持
 *     compositor surface，CDP 截图稳定可用）。禁 detach / 零尺寸 bounds / 屏外坐标；
 *     rect 无效时只切 z 序，bounds 保持最后一次有效值。
 *   - z 序不变量（底→顶）：可见态 [platformView, ...tabs（active 最顶）]；
 *     隐藏态 [...tabs, platformView]。settings 视图由 main.js 恒挂最顶（onRestack 回调）。
 *   - activeTabId = 可视激活（用户点标签/新建驱动）；AI 的 current tab 在 core
 *     store 里（browser tab N 只切命令目标），不动可视激活（不抢焦点语义）。
 *
 * currentSessionDir 由 browser-tool 在每次调用前设置（串行链保证无交叉）——
 * 下载产物的落盘目录（Go 后端下发的会话工作区）。
 */

import { WebContentsView, webContents, session } from "electron";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const __dirname = path.dirname(fileURLToPath(import.meta.url));

// 与插件同源的页内网络拦截器（fetch/XHR hook → window.__aic_network_logs）
const INTERCEPTOR_SRC = fs.readFileSync(
  path.join(__dirname, "vendor", "browser", "content", "network-interceptor.js"),
  "utf8",
);

const BG_PARTITION = "persist:aic-worker"; // AI 工作区（独立存储，不碰平台页默认会话）
const MAX_TABS = 50; // 标签数上限（防失控）

/**
 * host（main.js 注入，见 startBrowserServer）：
 *   win: BaseWindow 主窗口
 *   platformView: 平台页视图（z 序基准：隐藏态置顶遮挡 tabs）
 *   onChanged(state): 标签集变化推送（{tabs:[{id,title,url,loading}], activeTabId}，全量）
 *   onRestack(): tabs/platformView z 序重建后回调（main.js 用于恢复 settings 最顶）
 */
export function createElectronAdapter(host) {
  if (!host || !host.win) throw new Error("electron adapter requires host.win");
  if (!host.platformView) throw new Error("electron adapter requires host.platformView");

  // 工作区视图注册表：tabId → { view, winId: host.win.id }
  const tabs = new Map();
  // 已常驻 attach 的 tabId
  const attached = new Set();
  // ---- 内容池布局状态（渲染器驱动；docs §4）----
  let poolRect = null; // {x,y,width,height}，最后一次有效 rect（隐藏不清零）
  let poolVisible = false;
  let activeTabId = null; // 可视激活标签（内容区显示）
  let zRaised = false; // 当前 z 态：true = tabs 在 platformView 之上
  // 下载：一次性监听 + 完成历史（searchComplete 数据源）
  const createdListeners = new Set();
  const downloadHistory = [];
  // store（工作区 tab 状态）
  const storeMap = new Map();
  // 下载落盘目录（browser-tool 每次调用前设置，串行链保证无交叉）
  let currentSessionDir = null;

  // ---- 工作区视图基建 ----

  function partitionSession() {
    const ses = session.fromPartition(BG_PARTITION);
    if (!ses.__aicDownloadHooked) {
      ses.__aicDownloadHooked = true;
      ses.on("will-download", (_event, item) => {
        const dir = path.join(currentSessionDir || fallbackSessionDir(), ".browser");
        fs.mkdirSync(dir, { recursive: true, mode: 0o700 });
        const savePath = path.join(dir, sanitizeName(item.getFilename()));
        item.setSavePath(savePath);
        const rec = { filename: savePath, state: "in_progress" };
        downloadHistory.push(rec);
        // 历史只作 searchComplete 数据源，截断防长会话无上限积累
        if (downloadHistory.length > 100) downloadHistory.splice(0, downloadHistory.length - 100);
        for (const fn of createdListeners) fn({ filename: savePath });
        item.once("done", (_e, state) => {
          rec.state = state === "completed" ? "complete" : state;
        });
      });
    }
    return ses;
  }

  // 下载兜底目录（正常路径是 Go 下发的 session_dir；不可得时落用户公共区）
  // os.homedir() 跨平台（process.env.HOME 在 Windows 未定义）
  function fallbackSessionDir() {
    return path.join(os.homedir() || ".", ".aic", "sessions", "_default");
  }

  function sanitizeName(name) {
    return String(name || "download.bin").replace(/[\\/]/g, "_");
  }

  function attachView(view) {
    const wc = view.webContents;
    // 常驻 CDP：注入网络拦截器（与插件 content script 同源的 MAIN 世界 document_start）
    try {
      wc.debugger.attach("1.3");
      attached.add(wc.id);
      wc.debugger
        .sendCommand("Page.addScriptToEvaluateOnNewDocument", { source: INTERCEPTOR_SRC })
        .catch(() => {});
    } catch { /* debugger 占用等：首个命令时再 attach */ }
  }

  // ---- 内容池：rect 校验 / z 序 / 状态推送 ----

  const contentView = () => host.win.contentView;

  // rect 数值校验 + clamp 进 content 区（docs §4.4）；非法 → null（调用方只切 z 不动 bounds）
  function sanitizeRect(r) {
    if (!r || typeof r !== "object") return null;
    let { x, y, w, h } = r;
    x = Number(x); y = Number(y); w = Number(w); h = Number(h);
    if (![x, y, w, h].every(Number.isFinite)) return null;
    const [cw, ch] = host.win.getContentSize();
    x = Math.max(0, Math.min(Math.round(x), Math.max(0, cw - 2)));
    y = Math.max(0, Math.min(Math.round(y), Math.max(0, ch - 2)));
    w = Math.max(0, Math.min(Math.round(w), cw - x));
    h = Math.max(0, Math.min(Math.round(h), ch - y));
    return { x, y, width: w, height: h };
  }

  // 未推过 rect 时的回落：全内容区（隐藏态 CDP 截图保持历史语义 = 整窗尺寸）
  function effectiveRect() {
    if (poolRect) return poolRect;
    if (host.win.isDestroyed()) return null;
    const [w, h] = host.win.getContentSize();
    return { x: 0, y: 0, width: w, height: h };
  }

  function poolShown() {
    return !!(
      poolVisible && poolRect &&
      poolRect.width >= 2 && poolRect.height >= 2 &&
      activeTabId != null && tabs.has(activeTabId)
    );
  }

  // 目标 z 序重建（底→顶）：可见 [platform, ...tabs(active 末位)] / 隐藏 [...tabs, platform]。
  // 先摘后挂（已挂载视图重复 addChildView 会失败）；settings 不在本序内，main.js
  // 经 onRestack 恢复其最顶。
  function rebuildOrder(show) {
    const cv = contentView();
    const active = tabs.get(activeTabId)?.view || null;
    const others = [...tabs.values()].map((t) => t.view).filter((v) => v !== active);
    const ordered = show
      ? [host.platformView, ...others, ...(active ? [active] : [])]
      : [...others, ...(active ? [active] : []), host.platformView];
    for (const v of ordered) if (cv.children.includes(v)) cv.removeChildView(v);
    for (const v of ordered) cv.addChildView(v);
  }

  // force = 标签集/激活变化（重建序）；否则仅可见态翻转时重建，bounds 每次同步
  function syncZ(force = false) {
    if (host.win.isDestroyed()) return;
    const show = poolShown();
    const r = effectiveRect();
    if (r) {
      for (const { view } of tabs.values()) {
        if (!view.webContents.isDestroyed()) view.setBounds(r);
      }
    }
    if (!force && show === zRaised) return;
    zRaised = show;
    rebuildOrder(show);
    try { host.onRestack?.(); } catch { /* settings 未建 */ }
  }

  function state() {
    const out = [];
    for (const { view } of tabs.values()) {
      const wc = view.webContents;
      out.push({ id: wc.id, title: wc.getTitle(), url: wc.getURL(), loading: wc.isLoading() });
    }
    return { tabs: out, activeTabId };
  }

  function emitChanged() {
    try { host.onChanged?.(state()); } catch { /* 渲染器重建中：getState 是权威源 */ }
  }

  const lastTabId = () => {
    const keys = [...tabs.keys()];
    return keys.length ? keys[keys.length - 1] : null;
  };

  // 创建 AI 工作区标签页视图（不挂树：z 序由 syncZ 统一重建，bounds 由 effectiveRect 给）
  function createView(url) {
    const view = new WebContentsView({
      webPreferences: {
        session: partitionSession(),
        sandbox: true,
        contextIsolation: true,
        nodeIntegration: false,
      },
    });
    const wc = view.webContents;
    const tabId = wc.id;
    tabs.set(tabId, { view, winId: host.win.id });
    attachView(view);
    wc.on("page-title-updated", emitChanged);
    wc.on("did-navigate", emitChanged);
    wc.on("did-navigate-in-page", emitChanged);
    wc.on("did-start-loading", emitChanged);
    wc.on("did-stop-loading", emitChanged);
    wc.on("render-process-gone", emitChanged); // 崩溃不删标签（用户可原地重载/关闭），只同步状态
    // core onUpdated 契约（actWait 等加载等待的数据源）
    wc.on("did-finish-load", () => {
      for (const fn of updatedListeners) fn(tabId, { status: "complete" });
    });
    wc.on("destroyed", () => {
      tabs.delete(tabId);
      attached.delete(tabId);
      if (activeTabId === tabId) activeTabId = lastTabId();
      syncZ(true);
      emitChanged();
    });
    // 新窗口请求（target=_blank / window.open）→ 转新标签；非 http(s) 协议拒绝
    wc.setWindowOpenHandler(({ url: target }) => {
      if (/^https?:\/\//i.test(target) || target === "about:blank") {
        tabsCreate({ url: target }).catch(() => { /* 上限/销毁竞态 */ });
      }
      return { action: "deny" };
    });
    if (url) wc.loadURL(url).catch(() => {});
    return view;
  }

  // tabs.create 的公共内核（adapter 契约与 window.open 转发共用）：新建即可视激活
  async function tabsCreate({ windowId, url } = {}) {
    if (windowId !== undefined && String(windowId) !== String(host.win.id)) {
      throw new Error(`window ${windowId} not found`);
    }
    if (tabs.size >= MAX_TABS) throw new Error(`tab limit reached (${MAX_TABS})`);
    const view = createView(url || "about:blank");
    activeTabId = view.webContents.id;
    syncZ(true);
    emitChanged();
    return tabOf(view.webContents.id);
  }

  async function tabOf(tabId) {
    const entry = tabs.get(tabId);
    if (!entry) throw new Error(`tab ${tabId} not found`);
    const wc = webContents.fromId(tabId);
    if (!wc || wc.isDestroyed()) throw new Error(`tab ${tabId} not found`);
    return {
      id: tabId,
      windowId: entry.winId,
      title: wc.getTitle(),
      url: wc.getURL(),
      active: tabId === activeTabId, // 可视激活（docs §3：与 AI current tab 分离）
    };
  }

  const updatedListeners = new Set();

  // ---- adapter 契约 ----

  return {
    // browser-tool 每次调用前设置（下载产物落盘目录）
    setSessionDir(dir) { currentSessionDir = dir || null; },

    // 内容池控制面（main.js native:* IPC 调用）
    tabControl: {
      // 渲染器推送布局：rect 非空才更新（null = 只改可见性，bounds 保持最后有效值）
      applyLayout({ rect, visible } = {}) {
        if (rect !== undefined && rect !== null) {
          const r = sanitizeRect(rect);
          if (r) poolRect = r;
        }
        if (typeof visible === "boolean") poolVisible = visible;
        syncZ(false);
      },
      setActive(id) {
        const tid = Number(id);
        if (!tabs.has(tid)) return;
        activeTabId = tid;
        syncZ(true);
        emitChanged();
      },
      hasWorkTabs: () => tabs.size > 0,
      getState: state,
    },

    tabs: {
      async active() {
        throw new Error("coop mode is not available on desktop (AI works in the AI workspace tab)");
      },
      async get(id) {
        return tabOf(Number(id));
      },
      async list(windowId) {
        if (windowId === undefined) return []; // 协作模式不在 desktop 提供
        if (String(windowId) !== String(host.win.id)) return [];
        // core 直接读 t.title/t.url：必须解出对象数组（tabOf 是 async）
        return Promise.all([...tabs.keys()].map((id) => tabOf(id)));
      },
      async create({ windowId, url, active }) {
        void active; // 标签页语义：不激活窗口焦点（可视激活由 activeTabId 承担）
        return tabsCreate({ windowId, url });
      },
      async update(id, props) {
        const wc = webContents.fromId(Number(id));
        if (!wc) throw new Error(`tab ${id} not found`);
        if (props.url) await wc.loadURL(props.url);
        // {active} 在标签页语义下无意义（不激活窗口），忽略
        return tabOf(Number(id));
      },
      async remove(id) {
        const entry = tabs.get(Number(id));
        if (!entry) return;
        tabs.delete(Number(id));
        attached.delete(Number(id));
        if (!entry.view.webContents.isDestroyed()) {
          const cv = contentView();
          if (cv.children.includes(entry.view)) cv.removeChildView(entry.view);
          entry.view.webContents.close(); // 触发 'destroyed'（map 已删，回调幂等）
        }
        if (activeTabId === Number(id)) activeTabId = lastTabId();
        syncZ(true);
        emitChanged();
      },
      onUpdated: {
        add(fn) { updatedListeners.add(fn); },
        remove(fn) { updatedListeners.delete(fn); },
      },
    },

    windows: {
      async listNormal() {
        // desktop 无用户窗口：AI 工作区即主窗口的一个标签页
        return [{ id: host.win.id, incognito: false }];
      },
      async get(id) {
        if (String(id) !== String(host.win.id)) throw new Error(`window ${id} not found`);
        return { id, incognito: false };
      },
      async createIncognitoUnfocused() {
        throw new Error("incognito mode is not available on desktop (settings fix background:true)");
      },
    },

    async evalIn(tabId, func, args) {
      const wc = webContents.fromId(Number(tabId));
      if (!wc) throw new Error(`tab ${tabId} not found`);
      // 与 chrome.scripting 的 func+args 结构化语义等价：函数体序列化 + JSON 参数
      const code = `(${func.toString()})(${(args || []).map((a) => JSON.stringify(a)).join(",")})`;
      return wc.executeJavaScript(code, true);
    },

    cdp: {
      async attach(tabId) {
        tabId = Number(tabId);
        if (attached.has(tabId)) return; // 常驻 attach：幂等
        const wc = webContents.fromId(tabId);
        if (!wc) throw new Error(`tab ${tabId} not found`);
        try {
          wc.debugger.attach("1.3");
        } catch (e) {
          throw new Error(`debugger attach failed: ${e?.message || e}`);
        }
        attached.add(tabId);
      },
      async send(tabId, method, params) {
        const wc = webContents.fromId(Number(tabId));
        if (!wc) throw new Error(`tab ${tabId} not found`);
        // 隐藏标签页模型：AI 视图常驻窗口底层（被平台页遮挡），实测被遮挡视图
        // 依然有 compositor surface——CDP 截图直接可用，无需显示切换。
        return wc.debugger.sendCommand(method, params);
      },
      async detach() { /* 常驻 attach 语义：detach 为 no-op */ },
    },

    downloads: {
      onceCreated(fn) {
        createdListeners.add(fn);
        return () => createdListeners.delete(fn);
      },
      async searchComplete(escapedRegex) {
        const re = new RegExp(escapedRegex);
        return downloadHistory.filter((d) => d.state === "complete" && re.test(d.filename));
      },
    },

    store: {
      async get(key) { return storeMap.get(key) ?? null; },
      async set(key, value) { storeMap.set(key, value ?? null); },
    },

    async settings() {
      // desktop 固定后台工作区模式（AI 工作区标签页；不提供协作/无痕开关）
      return { incognito: false, background: true };
    },
  };
}
