/**
 * electron-adapter.mjs — browser core 的 desktop（Electron）宿主适配器（主窗口标签页模型）
 *
 * 与 Chrome 插件适配器的映射关系（core.js adapter 契约）：
 *   tabs     → 主窗口内的 WebContentsView（AI 工作区标签页，webContents.id 即 tabId）
 *   windows  → 主窗口本身（BaseWindow）：desktop 无用户窗口概念，AI 工作区以
 *              「AI 工作区」标签页形式与「平台」标签共同存在于主窗口（标签栏切换）
 *   evalIn   → webContents.executeJavaScript（func.toString() + JSON 参数序列化）
 *   cdp      → webContents.debugger（常驻 attach；网络拦截器经
 *              Page.addScriptToEvaluateOnNewDocument 注入，与插件 content script 同源）
 *   downloads→ partition session 的 will-download（产物落会话目录 .browser/）
 *   store    → 主进程内存 Map（Electron 主进程无休眠，工作区状态无需持久化）
 *   settings → 固定 { background: true }（desktop 不提供协作模式：AI 一律在
 *              AI 工作区标签页作业；平台标签页操作有自指风险）
 *
 * 标签可见性（A/B 分区模型，2026-09-10）：B 区收起时 AI 视图挂 contentView
 * 底层，被平台页完全遮挡（隐藏标签页；实测被遮挡的 WebContentsView 依然有
 * compositor surface，CDP 截图稳定可用——零闪现）；标签创建 → main.js 自动
 * 展开右侧 B 区，contentBounds() 返回 B 矩形，标签在 B 区可见；全部关闭 →
 * B 收起回落遮挡隐藏。showAiTab/hideAiTab 即两种态的 z 序切换。
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

/**
 * host（main.js 注入，见 startBrowserServer）：
 *   win: BaseWindow 主窗口
 *   contentBounds(): () => {x,y,width,height} AI 标签 bounds——B 展开时为右侧 B
 *                  矩形（标签可见），收起时为全内容区（平台页遮挡 = 隐藏）
 *   raisePlatform(): 平台页视图置顶（AI 视图挂入/隐藏后恢复遮挡）
 *   onTabsChanged(n): 标签数变化回调（main.js 驱动 B 区自动开合）
 */
export function createElectronAdapter(host) {
  if (!host || !host.win) throw new Error("electron adapter requires host.win");

  // 工作区视图注册表：tabId → { view, winId: host.win.id }
  const tabs = new Map();
  // 已常驻 attach 的 tabId
  const attached = new Set();
  // 当前是否显示 AI 标签页（view 是否在窗口 contentView 上）
  let aiVisible = false;
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

  // 内容区布局（全窗口：平台页与 AI 工作区共用；标签栏移除后无预留）
  function boundsToContentArea() {
    return host.contentBounds ? host.contentBounds() : null;
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

  // 创建 AI 工作区标签页视图（挂到主窗口 contentView 底层，被平台页遮挡 = 隐藏）
  function createView(url) {
    const view = new WebContentsView({
      webPreferences: {
        session: partitionSession(),
        sandbox: true,
        contextIsolation: true,
        nodeIntegration: false,
      },
    });
    const tabId = view.webContents.id;
    tabs.set(tabId, { view, winId: host.win.id });
    attachView(view);
    view.webContents.on("did-finish-load", () => {
      for (const fn of updatedListeners) fn(tabId, { status: "complete" });
    });
    view.webContents.on("destroyed", () => {
      tabs.delete(tabId);
      attached.delete(tabId);
    });
    if (url) view.webContents.loadURL(url).catch(() => {});
    return view;
  }

  // 标签 z 顺序：contentView 后加入的 view 在上层（调用方负责先摘下已挂载 view）。
  // 隐藏态 = AI 视图在底层（平台页遮挡）；显示态 = AI 视图提升到平台页之上。
  function applyZ() {
    const b = boundsToContentArea();
    for (const { view } of tabs.values()) {
      if (b) view.setBounds(b);
      host.win.contentView.addChildView(view);
    }
  }

  // 标签 z 序切换前一律先摘后挂（contentView 上已挂载的 view 重复 addChildView 会失败）
  function showAiTab() {
    if (aiVisible) return;
    for (const { view } of tabs.values()) {
      host.win.contentView.removeChildView(view);
    }
    applyZ(); // AI 视图重新挂入并 setBounds（B 展开 = B 矩形，顶层可见）
    aiVisible = true;
  }

  function hideAiTab() {
    if (!aiVisible) return;
    // AI 视图回落底层：先摘再按当前 contentBounds 重挂（B 已收起 = 全尺寸），
    // 最后置顶平台页——终态平台页在最上，AI 视图被遮挡（隐藏）。
    for (const { view } of tabs.values()) {
      host.win.contentView.removeChildView(view);
    }
    applyZ();
    if (host.raisePlatform) host.raisePlatform();
    aiVisible = false;
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
      active: false, // 标签页语义：不激活窗口
    };
  }

  const updatedListeners = new Set();

  // ---- adapter 契约 ----

  return {
    // browser-tool 每次调用前设置（下载产物落盘目录）
    setSessionDir(dir) { currentSessionDir = dir || null; },

    // B 区开合控制（main.js setBOpen / layoutMain 调用）
    tabControl: {
      show: showAiTab,
      hide: hideAiTab,
      hasWorkTabs: () => tabs.size > 0,
      // 窗口 resize / B 区拖拽后按当前 contentBounds 重设所有标签 bounds（z 序不动）
      relayout() {
        const b = boundsToContentArea();
        if (!b) return;
        for (const { view } of tabs.values()) view.setBounds(b);
      },
    },

    tabs: {
      async active() {
        throw new Error("coop mode is not available on desktop (AI works in the AI workspace tab)");
      },
      async get(id) {
        return tabOf(id);
      },
      async list(windowId) {
        if (windowId === undefined) return []; // 协作模式不在 desktop 提供
        if (String(windowId) !== String(host.win.id)) return [];
        return [...tabs.keys()].map((id) => tabOf(id));
      },
      async create({ windowId, url, active }) {
        void active; // 标签页语义：不激活窗口焦点
        if (String(windowId) !== String(host.win.id)) {
          throw new Error(`window ${windowId} not found`);
        }
        const view = createView(url || "about:blank");
        // 挂入 contentView（B 收起时底层遮挡；contentBounds 由 host 决定）
        {
          const b = boundsToContentArea();
          if (b) view.setBounds(b);
          host.win.contentView.addChildView(view);
          if (host.raisePlatform) host.raisePlatform(); // 平台页置顶（B 收起 = 遮挡隐藏）
        }
        host.onTabsChanged?.(tabs.size); // 首个标签 → main.js 自动展开 B 区
        return tabOf(view.webContents.id);
      },
      async update(id, props) {
        const wc = webContents.fromId(id);
        if (!wc) throw new Error(`tab ${id} not found`);
        if (props.url) await wc.loadURL(props.url);
        // {active} 在标签页语义下无意义（不激活窗口），忽略
        return tabOf(id);
      },
      async remove(id) {
        const entry = tabs.get(id);
        if (!entry) return;
        const wc = webContents.fromId(id);
        if (entry.view && !entry.view.webContents.isDestroyed()) {
          host.win.contentView.removeChildView(entry.view);
        }
        tabs.delete(id);
        attached.delete(id);
        if (wc) wc.close();
        // 最后一个工作区 tab 被关 → 回落平台页遮挡态（main.js 经 onTabsChanged 收起 B）
        if (tabs.size === 0 && aiVisible) hideAiTab();
        host.onTabsChanged?.(tabs.size);
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
      const wc = webContents.fromId(tabId);
      if (!wc) throw new Error(`tab ${tabId} not found`);
      // 与 chrome.scripting 的 func+args 结构化语义等价：函数体序列化 + JSON 参数
      const code = `(${func.toString()})(${(args || []).map((a) => JSON.stringify(a)).join(",")})`;
      return wc.executeJavaScript(code, true);
    },

    cdp: {
      async attach(tabId) {
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
        const wc = webContents.fromId(tabId);
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
