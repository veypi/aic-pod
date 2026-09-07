/**
 * electron-adapter.js — browser core 的 desktop（Electron）宿主适配器
 *
 * 与 Chrome 插件适配器的映射关系（core.js adapter 契约）：
 *   tabs     → WebContentsView（工作区窗口内的子视图，webContents.id 即 tabId）
 *   windows  → 隐藏 BrowserWindow（show:false；background=persist:aic-worker
 *              partition，incognito=内存 partition）；desktop 无用户窗口概念，
 *              listNormal 懒建后台工作区窗口
 *   evalIn   → webContents.executeJavaScript（func.toString() + JSON 参数序列化，
 *              与 chrome.scripting 的 func/args 结构化语义等价）
 *   cdp      → webContents.debugger（常驻 attach：创建 view 时挂上并注入
 *              network-interceptor（Page.addScriptToEvaluateOnNewDocument，
 *              与插件 content script 同源）；core 的 attach/detach 语义归一为幂等）
 *   downloads→ partition session 的 will-download（产物落会话目录 .browser/）
 *   store    → 主进程内存 Map（Electron 主进程无休眠，工作区状态无需持久化）
 *   settings → 固定 { background: true }（desktop 不提供协作模式：主窗口即平台页，
 *              操作它有自指风险；AI 一律在隐藏工作区窗口作业）
 *
 * currentSessionDir 由 browser-tool 在每次调用前设置（串行链保证无交叉）——
 * 下载产物的落盘目录（Go 后端下发的会话工作区）。
 */

import { BrowserWindow, WebContentsView, webContents, session } from "electron";
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

const BG_PARTITION = "persist:aic-worker"; // 后台工作区（独立存储，不碰主窗口 defaultSession）
const INCOG_PARTITION = "aic-worker-incog"; // 无痕工作区（内存 partition，无持久化）

export function createElectronAdapter() {
  // 工作区窗口注册表：winId → { win, incognito, views: Map<tabId, WebContentsView> }
  const wins = new Map();
  // tabId → winId 反查
  const tabWin = new Map();
  // 已常驻 attach 的 tabId
  const attached = new Set();
  // 下载：一次性监听 + 完成历史（searchComplete 数据源）
  const createdListeners = new Set();
  const downloadHistory = [];
  // store（工作区 tab 状态）
  const storeMap = new Map();
  // 下载落盘目录（browser-tool 每次调用前设置，串行链保证无交叉）
  let currentSessionDir = null;

  // ---- 工作区窗口/view 基建 ----

  function partitionSession(incognito) {
    const ses = session.fromPartition(incognito ? INCOG_PARTITION : BG_PARTITION);
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

  function ensureWorkspaceWindow(incognito) {
    for (const [id, w] of wins) {
      if (w.incognito === incognito && !w.win.isDestroyed()) return w;
    }
    const ses = partitionSession(incognito);
    const win = new BrowserWindow({
      show: false, // 隐藏工作区：绝不与用户抢焦点
      width: 1280,
      height: 800,
      webPreferences: { sandbox: true },
    });
    const entry = { win, incognito, views: new Map(), ses };
    wins.set(win.id, entry);
    win.on("closed", () => {
      for (const tabId of entry.views.keys()) {
        tabWin.delete(tabId);
        attached.delete(tabId);
      }
      wins.delete(win.id);
    });
    return entry;
  }

  function createView(entry, url) {
    const view = new WebContentsView({
      webPreferences: {
        session: entry.ses,
        sandbox: true,
        contextIsolation: true,
        nodeIntegration: false,
      },
    });
    entry.win.contentView.addChildView(view);
    view.setBounds({ x: 0, y: 0, width: 1280, height: 800 });
    const tabId = view.webContents.id;
    entry.views.set(tabId, view);
    tabWin.set(tabId, entry.win.id);
    // 常驻 CDP：注入网络拦截器（与插件 content script 同源的 MAIN 世界 document_start）
    try {
      view.webContents.debugger.attach("1.3");
      attached.add(tabId);
      view.webContents.debugger
        .sendCommand("Page.addScriptToEvaluateOnNewDocument", { source: INTERCEPTOR_SRC })
        .catch(() => {});
    } catch { /* debugger 占用等：首个命令时再 attach */ }
    view.webContents.on("did-finish-load", () => {
      for (const fn of updatedListeners) fn(tabId, { status: "complete" });
    });
    view.webContents.on("destroyed", () => {
      entry.views.delete(tabId);
      tabWin.delete(tabId);
      attached.delete(tabId);
    });
    if (url) view.webContents.loadURL(url).catch(() => {});
    return view;
  }

  function tabOf(tabId) {
    const wc = webContents.fromId(tabId);
    if (!wc) throw new Error(`tab ${tabId} not found`);
    return {
      id: tabId,
      windowId: tabWin.get(tabId),
      title: wc.getTitle(),
      url: wc.getURL(),
      active: false, // 工作区永不激活
    };
  }

  const updatedListeners = new Set();

  // ---- adapter 契约 ----

  return {
    // browser-tool 每次调用前设置（下载产物落盘目录）
    setSessionDir(dir) { currentSessionDir = dir || null; },

    tabs: {
      async active() {
        throw new Error("coop mode is not available on desktop (workspace is a hidden window)");
      },
      async get(id) {
        return tabOf(id);
      },
      async list(windowId) {
        if (windowId === undefined) return []; // 协作模式不在 desktop 提供
        const w = wins.get(windowId);
        if (!w) return [];
        return [...w.views.keys()].map((id) => tabOf(id));
      },
      async create({ windowId, url, active }) {
        void active; // desktop 工作区永不激活
        const w = wins.get(windowId);
        if (!w) throw new Error(`window ${windowId} not found`);
        const view = createView(w, url || "about:blank");
        return tabOf(view.webContents.id);
      },
      async update(id, props) {
        const wc = webContents.fromId(id);
        if (!wc) throw new Error(`tab ${id} not found`);
        if (props.url) await wc.loadURL(props.url);
        // {active} 在 desktop 无意义（隐藏工作区），忽略
        return tabOf(id);
      },
      async remove(id) {
        const winId = tabWin.get(id);
        const w = wins.get(winId);
        const view = w?.views.get(id);
        if (w && view) {
          w.win.contentView.removeChildView(view);
          view.webContents.close();
          w.views.delete(id);
          tabWin.delete(id);
          attached.delete(id);
        }
      },
      onUpdated: {
        add(fn) { updatedListeners.add(fn); },
        remove(fn) { updatedListeners.delete(fn); },
      },
    },

    windows: {
      async listNormal() {
        // desktop 无用户窗口：懒建后台工作区窗口（core 的 findNormalWindow 依赖非空）
        ensureWorkspaceWindow(false);
        return [...wins.values()]
          .filter((w) => !w.win.isDestroyed())
          .map((w) => ({ id: w.win.id, incognito: w.incognito }));
      },
      async get(id) {
        const w = wins.get(id);
        if (!w || w.win.isDestroyed()) throw new Error(`window ${id} not found`);
        return { id, incognito: w.incognito };
      },
      async createIncognitoUnfocused() {
        const w = ensureWorkspaceWindow(true);
        // core 语义：无痕窗口自带一张 about:blank tab（tabs[0] 复用）
        if (w.views.size === 0) createView(w, "about:blank");
        return { id: w.win.id };
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
      // desktop 固定后台工作区模式（不提供协作/无痕开关，见文件头注释）
      return { incognito: false, background: true };
    },
  };
}
