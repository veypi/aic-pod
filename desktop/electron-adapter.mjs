/** Electron tab pool plus CDP transport for ui/1. The native content layout is
 * controlled by the platform renderer; automation never activates the OS window. */
import { WebContentsView, webContents, session, nativeImage } from "electron";
import crypto from "node:crypto";
import { createEventLog } from "./browser/network.mjs";
import fs from "node:fs";
import path from "node:path";

const BG_PARTITION = "persist:aic-worker"; // AI 工作区（独立存储，不碰平台页默认会话）
const MAX_TABS = 50; // 标签数上限（防失控）

/**
 * host（main.js 注入，见 startBrowserServer）：
 *   win: BaseWindow 主窗口
 *   platformView: 平台页视图（z 序基准：恒最顶，洞由页面 mask 表达）
 *   onChanged(state): 标签集变化推送（{tabs:[{id,title,url,loading}], activeTabId}，全量）
 *   onRestack(): tabs/platformView z 序重建后回调（main.js 用于恢复 platform 最顶）
 *   onTabView(wc): 新标签视图创建回调（main.js 用于挂 leader 键抓取；adapter 不感知语义）
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
  // 下载只在显式授权的 download 调用内落盘。
  const pendingDownloads = new Map();
  const eventLogs = new Map();
  const revisions = new Map();
  const dialogs = new Map(), dialogListeners = new Set();

  // ---- 工作区视图基建 ----

  function partitionSession() {
    const ses = session.fromPartition(BG_PARTITION);
    if (!ses.__aicDownloadHooked) {
      ses.__aicDownloadHooked = true;
      ses.on("will-download", (event, item, wc) => {
        const pending = pendingDownloads.get(wc?.id);
        if (!pending) { event.preventDefault(); return; }
        pendingDownloads.delete(wc.id);
        fs.mkdirSync(pending.dir, { recursive: true, mode: 0o700 });
        const name = crypto.randomBytes(8).toString("hex") + "-" + sanitizeName(item.getFilename());
        const destination = path.join(pending.dir, name);
        item.setSavePath(destination);
        pending.item = item;
        item.once("done", (_e, state) => {
          if (state === "completed") pending.resolve({ path: destination, bytes: item.getReceivedBytes(), mime: item.getMimeType() });
          else pending.reject(new Error(`download ${state}`));
        });
      });
    }
    return ses;
  }

  function sanitizeName(name) {
    return String(name || "download.bin").replace(/[\\/]/g, "_");
  }

  function attachView(view) {
    const wc = view.webContents;
    eventLogs.set(wc.id, createEventLog());
    revisions.set(wc.id, 1);
    wc.debugger.on("message", (_e, method, params) => {
      eventLogs.get(wc.id)?.event(method, params);
      if (method === "Page.javascriptDialogOpening" || method === "Page.javascriptDialogClosed") {
        const dialog = method.endsWith("Opening") ? {type:params.type,message:params.message,url:params.url,default_prompt:params.defaultPrompt || ""} : null;
        if (dialog) dialogs.set(wc.id,dialog); else dialogs.delete(wc.id);
        for (const listener of dialogListeners) listener({tab:wc.id,dialog});
      }
      if (method === "DOM.documentUpdated" || method.startsWith("DOM.childNode") || method === "DOM.attributeModified" || method === "DOM.attributeRemoved" || method === "DOM.characterDataModified" || method === "Page.frameNavigated") {
        revisions.set(wc.id, (revisions.get(wc.id) || 0) + 1);
      }
    });
    wc.debugger.on("detach", () => { attached.delete(wc.id); dialogs.delete(wc.id); revisions.set(wc.id, (revisions.get(wc.id) || 0) + 1); });
    try {
      wc.debugger.attach("1.3");
      attached.add(wc.id);
      for (const domain of ["Page", "DOM", "Accessibility", "Network", "Runtime"]) wc.debugger.sendCommand(domain + ".enable").catch(() => {});
      wc.debugger.sendCommand("Emulation.setFocusEmulationEnabled", {enabled:true}).catch(() => {});
    } catch { /* attach is retried by the first explicit operation */ }
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

  // 成员重排（v2）：tabs 池内 active 恒最顶；重排后由 onRestack 抬回 platform
  //（不变量：底→顶 […tabs, platformView]）。先摘后挂（已挂载视图重复
  // addChildView 会失败）；platform 不在本序内。
  function restackTabs() {
    const cv = contentView();
    const active = tabs.get(activeTabId)?.view || null;
    const others = [...tabs.values()].map((t) => t.view).filter((v) => v !== active);
    const ordered = [...others, ...(active ? [active] : [])];
    for (const v of ordered) if (cv.children.includes(v)) cv.removeChildView(v);
    for (const v of ordered) cv.addChildView(v);
  }

  // force = 标签集/激活变化（重排）；否则仅 bounds 同步（可见性由页面 mask 表达）
  function syncZ(force = false) {
    if (host.win.isDestroyed()) return;
    const r = effectiveRect();
    if (r) {
      for (const { view } of tabs.values()) {
        if (!view.webContents.isDestroyed()) view.setBounds(r);
      }
    }
    if (!force) return;
    restackTabs();
    try { host.onRestack?.(); } catch { /* platform 未就绪 */ }
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

  // 创建 AI 工作区标签页视图（不挂树：成员重排由 syncZ 统一处理，bounds 由 effectiveRect 给）
  function createView(url) {
    const view = new WebContentsView({
      webPreferences: {
        session: partitionSession(),
        sandbox: true,
        contextIsolation: true,
        nodeIntegration: false,
        backgroundThrottling: false,
      },
    });
    const wc = view.webContents;
    host.onTabView?.(wc); // 壳侧 leader 键抓取挂载（main.js；壳能力，adapter 不感知语义）
    const tabId = wc.id;
    tabs.set(tabId, { view, winId: host.win.id });
    attachView(view);
    wc.on("page-title-updated", emitChanged);
    wc.on("did-navigate", emitChanged);
    wc.on("did-navigate-in-page", emitChanged);
    wc.on("did-start-loading", emitChanged);
    wc.on("did-stop-loading", emitChanged);
    wc.on("render-process-gone", emitChanged); // 崩溃不删标签（用户可原地重载/关闭），只同步状态
    wc.on("destroyed", () => {
      tabs.delete(tabId);
      attached.delete(tabId);
      eventLogs.delete(tabId); revisions.delete(tabId); dialogs.delete(tabId);
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


  // ---- adapter 契约 ----

  return {
    // browser-tool 每次调用前设置（下载产物落盘目录）
    revision(tabId) { return revisions.get(tabId) || 0; },
    dialog(tabId) { return dialogs.get(tabId); },
    onDialog(listener) { dialogListeners.add(listener); return () => dialogListeners.delete(listener); },
    events(tabId) { return eventLogs.get(tabId); },
    async encodeImage(bytes) {
      const original = nativeImage.createFromBuffer(bytes);
      if (original.isEmpty()) throw new Error("invalid screenshot image");
      const size = original.getSize();
      if (bytes.length <= 600 * 1024) return { bytes, mime: "image/png", ...size };
      for (let scale = 1; scale >= 1 / 32; scale /= 2) {
        const img = original.resize({ width: Math.max(1, Math.round(size.width * scale)) });
        for (const quality of [80, 60, 40]) {
          const out = img.toJPEG(quality);
          if (out.length <= 600 * 1024) return { bytes: out, mime: "image/jpeg", ...img.getSize(), note: `${bytes.length} bytes → image/jpeg ${img.getSize().width}x${img.getSize().height} quality ${quality} (${out.length} bytes)` };
        }
      }
      throw new Error("screenshot exceeds image budget");
    },
    async download(tabId, dir, trigger, deadline) {
      if (pendingDownloads.has(tabId)) throw new Error("download already pending for this tab");
      let pending;
      const completed = new Promise((resolve, reject) => { pending = { dir, resolve, reject, item: null }; });
      // Observe rejection even if triggering the click itself fails.
      completed.catch(() => {});
      const timer = setTimeout(() => { pendingDownloads.delete(tabId); pending.item?.cancel(); pending.reject(new Error("download timed out; trigger may have executed")); }, Math.max(1, deadline - Date.now()));
      pendingDownloads.set(tabId, pending);
      try { await trigger(); return await completed; }
      finally { clearTimeout(timer); if (pendingDownloads.get(tabId) === pending) pendingDownloads.delete(tabId); }
    },

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
      // v2 输入路由（main.js）：活动 tab 的洞（可见才返回）；null = 该处无洞
      poolState() {
        if (!poolShown()) return null;
        const active = tabs.get(activeTabId);
        if (!active || active.view.webContents.isDestroyed()) return null;
        return { wc: active.view.webContents, rect: poolRect };
      },
    },

    tabs: {

      async get(id) {
        return tabOf(Number(id));
      },
      async list(windowId) {
        if (windowId !== undefined && String(windowId) !== String(host.win.id)) return [];
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
        eventLogs.delete(Number(id));
        revisions.delete(Number(id));
        if (!entry.view.webContents.isDestroyed()) {
          const cv = contentView();
          if (cv.children.includes(entry.view)) cv.removeChildView(entry.view);
          entry.view.webContents.close(); // 触发 'destroyed'（map 已删，回调幂等）
        }
        if (activeTabId === Number(id)) activeTabId = lastTabId();
        syncZ(true);
        emitChanged();
      },
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
        for (const domain of ["Page", "DOM", "Accessibility", "Network", "Runtime"]) await wc.debugger.sendCommand(domain + ".enable");
        await wc.debugger.sendCommand("Emulation.setFocusEmulationEnabled", {enabled:true});
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

  };
}
