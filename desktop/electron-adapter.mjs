/** Fixed-size offscreen browser pool. The platform displays frames; its layout
 * never resizes the browser and automation never activates an OS window. */
import { BrowserWindow, webContents, session, nativeImage } from "electron";
import { createBrowserInput } from "./browser/input.mjs";
import { normalizeViewport, fitViewport } from "./browser/viewport.mjs";
import crypto from "node:crypto";
import { createEventLog } from "./browser/network.mjs";
import fs from "node:fs";
import path from "node:path";

const BG_PARTITION = "persist:aic-worker"; // AI 工作区（独立存储，不碰平台页默认会话）
const MAX_TABS = 50; // 标签数上限（防失控）

/** host: win (lifetime), getViewport()/viewport (new-tab default),
 * onChanged(state), onFrame(frame). No browser is attached to the host window. */
export function createElectronAdapter(host) {
  if (!host || !host.win) throw new Error("electron adapter requires host.win");

  // tabId → { window, viewport, winId }; windows are always hidden and offscreen.
  const tabs = new Map();
  // 已常驻 attach 的 tabId
  const attached = new Set();
  // ---- 内容池布局状态（渲染器驱动；docs §4）----
  let poolRect = null; // {x,y,width,height}，最后一次有效 rect（隐藏不清零）
  let poolVisible = false;
  let activeTabId = null; // Presentation selection, independent from the automation target.
  let frameSeq = 0, pendingFrame = null, dirtyFrame = false;
  let frameTimer = null;
  let disposed = false;
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

  // ---- 展示区域、帧推送与生命周期 ----

  // Preserve the complete presentation rect even when it is partly off screen.
  function sanitizeRect(r) {
    if (!r || typeof r !== "object") return null;
    const { x, y, w, h } = r;
    if (![x, y, w, h].every(Number.isFinite) || w < 0 || h < 0) return null;
    return { x, y, width: w, height: h };
  }

  function poolShown() {
    return !!(poolVisible && poolRect && poolRect.width >= 2 && poolRect.height >= 2 && tabs.has(activeTabId));
  }

  // At most one frame in flight. Hidden viewers do not accumulate IPC frames.
  function resetFrame() {
    clearTimeout(frameTimer); frameTimer = null; pendingFrame = null; dirtyFrame = false;
  }
  function refreshFrame() {
    resetFrame();
    const wc = tabs.get(activeTabId)?.window.webContents;
    if (poolShown() && wc && !wc.isDestroyed()) wc.invalidate();
  }
  function acknowledgeFrame(seq) {
    if (seq !== pendingFrame) return;
    clearTimeout(frameTimer); frameTimer = null; pendingFrame = null;
    if (dirtyFrame) {
      dirtyFrame = false;
      const wc = tabs.get(activeTabId)?.window.webContents;
      if (wc && !wc.isDestroyed()) wc.invalidate();
    }
  }
  function publishFrame(id, image) {
    if (disposed || id !== activeTabId || !poolShown() || !host.onFrame ||
        host.win.isDestroyed() || !host.win.isVisible() || host.win.isMinimized()) return;
    if (pendingFrame !== null) { dirtyFrame = true; return; }
    const seq = ++frameSeq;
    pendingFrame = seq;
    frameTimer = setTimeout(() => acknowledgeFrame(seq), 1000);
    frameTimer.unref?.();
    try { host.onFrame({ tabId: id, seq, ...tabs.get(id).viewport, data: image.toPNG() }); }
    catch { resetFrame(); }
  }
  for (const event of ["show", "restore"]) host.win.on(event, refreshFrame);
  host.win.once("closed", dispose);
  function dispose() {
    if (disposed) return;
    disposed = true;
    resetFrame();
    for (const event of ["show", "restore"]) host.win.removeListener(event, refreshFrame);
    host.win.removeListener("closed", dispose);
    for (const { window } of [...tabs.values()]) if (!window.isDestroyed()) window.destroy();
    tabs.clear();
  }

  function state() {
    const out = [];
    for (const { window, viewport } of tabs.values()) {
      const wc = window.webContents;
      out.push({ id: wc.id, title: wc.getTitle(), url: wc.getURL(), loading: wc.isLoading(), viewport: { ...viewport } });
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

  // The only place browser size is assigned. OS layout changes never reach here.
  function createView(url, viewport) {
    const view = new BrowserWindow({
      ...viewport, useContentSize: true, show: false, frame: false,
      resizable: false, movable: false, focusable: false, skipTaskbar: true,
      backgroundColor: "#ffffff",
      webPreferences: {
        session: partitionSession(),
        sandbox: true,
        contextIsolation: true,
        nodeIntegration: false,
        backgroundThrottling: false,
        offscreen: { deviceScaleFactor: 1 },
      },
    });
    const wc = view.webContents;
    wc.setFrameRate(30);
    wc.setVisualZoomLevelLimits(1, 1);
    const tabId = wc.id;
    tabs.set(tabId, { window: view, viewport, winId: host.win.id });
    wc.on("paint", (_event, _dirty, image) => publishFrame(tabId, image));
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
      refreshFrame();
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
  async function tabsCreate({ windowId, url, viewport } = {}) {
    if (windowId !== undefined && String(windowId) !== String(host.win.id)) {
      throw new Error(`window ${windowId} not found`);
    }
    if (tabs.size >= MAX_TABS) throw new Error(`tab limit reached (${MAX_TABS})`);
    const size = normalizeViewport(viewport || (await host.getViewport?.()) || host.viewport);
    if (disposed || host.win.isDestroyed()) throw new Error("browser pool closed");
    if (tabs.size >= MAX_TABS) throw new Error(`tab limit reached (${MAX_TABS})`);
    const view = createView(url || "about:blank", size);
    activeTabId = view.webContents.id;
    refreshFrame();
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
      active: tabId === activeTabId,
      viewport: { ...entry.viewport },
    };
  }


  function inputTarget() {
    if (!poolShown()) return null;
    const active = tabs.get(activeTabId);
    if (!active || active.window.webContents.isDestroyed()) return null;
    return { wc: active.window.webContents, rect: fitViewport(poolRect, active.viewport) };
  }
  const input = createBrowserInput(inputTarget);

  // ---- adapter 契约 ----

  return {
    dispose,
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
      input,
      // 渲染器只控制展示区域；null 不影响 browser 视口。
      applyLayout({ rect, visible, refresh = false } = {}) {
        const wasShown = poolShown();
        if (rect !== undefined && rect !== null) {
          const r = sanitizeRect(rect);
          if (r) poolRect = r;
        }
        if (typeof visible === "boolean") poolVisible = visible;
        if (!poolVisible) input.reset();
        if (refresh || wasShown !== poolShown()) refreshFrame();
      },
      acknowledgeFrame,
      setActive(id) {
        const tid = Number(id);
        if (!tabs.has(tid)) return;
        input.reset();
        activeTabId = tid;
        refreshFrame();
        emitChanged();
      },
      hasWorkTabs: () => tabs.size > 0,
      getState: state,
      // 可见图片的 contain 区域（留白不接收输入）。
      poolState: inputTarget,
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
      async create({ windowId, url, active, viewport }) {
        void active; // 标签页语义：不激活窗口焦点（可视激活由 activeTabId 承担）
        return tabsCreate({ windowId, url, viewport });
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
        if (!entry.window.isDestroyed()) entry.window.destroy();
        if (activeTabId === Number(id)) activeTabId = lastTabId();
        refreshFrame();
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
        // Offscreen pages keep their own compositor surface, including while the
        // host is minimized, hidden, or presenting another tab.
        return wc.debugger.sendCommand(method, params);
      },
      async detach() { /* 常驻 attach 语义：detach 为 no-op */ },
    },

  };
}
