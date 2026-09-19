/** Fixed-size offscreen browser pool. The platform displays frames; its layout
 * never resizes the browser and automation never activates an OS window. */
import { BrowserWindow, webContents, session, nativeImage } from "electron";
import { createBrowserInput } from "./browser/input.mjs";
import { normalizeViewport } from "./browser/viewport.mjs";
import crypto from "node:crypto";
import { createEventLog } from "./browser/network.mjs";
import fs from "node:fs";
import path from "node:path";

const BG_PARTITION = "persist:aic-worker"; // AI 工作区（独立存储，不碰平台页默认会话）
const MAX_TABS = 50; // 标签数上限（防失控）

/** host: win (process lifetime), getViewport()/viewport (new-window default).
 * Viewers discover resources through browser/1; there is no presentation layout. */
export function createElectronAdapter(host) {
  if (!host || !host.win) throw new Error("electron adapter requires host.win");

  // tabId → { window, viewport, winId }; windows are always hidden and offscreen.
  const tabs = new Map();
  // 已常驻 attach 的 tabId
  const attached = new Set();
  let disposed = false;
  // 下载只在显式授权的 download 调用内落盘。
  const pendingDownloads = new Map();
  const eventLogs = new Map();
  const revisions = new Map();
  const frameWatches = new Map();
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

  async function attachView(view) {
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
      for (const domain of ["Page", "DOM", "Accessibility", "Network", "Runtime"]) await wc.debugger.sendCommand(domain + ".enable");
      wc.debugger.sendCommand("Emulation.setFocusEmulationEnabled", {enabled:true}).catch(() => {});
    } catch (error) { throw new Error(`browser initialization failed: ${error.message}`); }
  }

  host.win.once("closed", dispose);
  function dispose() {
    if (disposed) return;
    disposed = true;
    host.win.removeListener("closed", dispose);
    for (const { window } of [...tabs.values()]) if (!window.isDestroyed()) window.destroy();
    tabs.clear();
  }

  // The only place browser size is assigned. OS layout changes never reach here.
  function createView(viewport) {
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
    // Tool-owned pages must never play audio on the device, including autoplay
    // and Web Audio. Mute the whole WebContents before its first navigation.
    wc.setAudioMuted(true);
    wc.setFrameRate(60);
    let repaintTimer, repaintDelay = 100;
    const retryPaint = () => {
      if (repaintTimer) return;
      repaintTimer = setTimeout(() => {
        repaintTimer = null;
        if (!wc.isDestroyed() && frameWatches.get(wc.id)?.size) wc.invalidate();
      }, repaintDelay);
      repaintDelay = Math.min(repaintDelay * 2, 1000);
    };
    wc.on("paint", (_event, _dirty, image) => {
      const watchers = frameWatches.get(wc.id);
      if (!watchers?.size) return;
      // A static page may never paint again after an empty startup frame.
      if (image.isEmpty()) return retryPaint();
      // Encode once per compositor frame, shared by all viewers of this target.
      const bytes = image.toJPEG(75);
      if (bytes.length < 4 || bytes[0] !== 0xff || bytes[1] !== 0xd8 ||
          bytes[bytes.length - 2] !== 0xff || bytes[bytes.length - 1] !== 0xd9) return retryPaint();
      clearTimeout(repaintTimer);
      repaintTimer = null;
      repaintDelay = 100;
      for (const watcher of watchers) watcher.frame(bytes);
    });
    wc.setVisualZoomLevelLimits(1, 1);
    const tabId = wc.id;
    tabs.set(tabId, { window: view, viewport, winId: host.win.id });
    let pageReady = Promise.resolve();
    wc.on("did-finish-load", () => {
      // Chromium gives about:blank a dark system canvas even when the window's
      // background is white. Style only that document; navigation drops the CSS.
      pageReady = wc.getURL() === "about:blank"
        ? wc.insertCSS(":root { color-scheme: light; background-color: #fff; }")
        : Promise.resolve();
      pageReady.then(() => {
        if (!wc.isDestroyed() && frameWatches.get(wc.id)?.size) wc.invalidate();
      }).catch(() => {});
    });
    // Start the renderer before awaiting CDP initialization. The target URL is
    // loaded only after CDP initialization and the blank document's styling.
    const initialLoad = wc.loadURL("about:blank").then(() => pageReady);
    const ready = Promise.all([attachView(view), initialLoad]);
    wc.on("destroyed", () => {
      clearTimeout(repaintTimer);
      for (const watcher of frameWatches.get(tabId) || []) watcher.closed();
      frameWatches.delete(tabId);
      tabs.delete(tabId);
      attached.delete(tabId);
      eventLogs.delete(tabId); revisions.delete(tabId); dialogs.delete(tabId);
    });
    // 新窗口请求（target=_blank / window.open）→ 转新标签；非 http(s) 协议拒绝
    wc.setWindowOpenHandler(({ url: target }) => {
      if (/^https?:\/\//i.test(target) || target === "about:blank") {
        tabsCreate({ url: target }).catch(() => { /* 上限/销毁竞态 */ });
      }
      return { action: "deny" };
    });
    return {view,ready};
  }

  // tabs.create and window.open share this device-owned pool.
  async function tabsCreate({ windowId, url, viewport } = {}) {
    if (windowId !== undefined && String(windowId) !== String(host.win.id)) {
      throw new Error(`window ${windowId} not found`);
    }
    if (tabs.size >= MAX_TABS) throw new Error(`tab limit reached (${MAX_TABS})`);
    const size = normalizeViewport(viewport || (await host.getViewport?.()) || host.viewport);
    if (disposed || host.win.isDestroyed()) throw new Error("browser pool closed");
    if (tabs.size >= MAX_TABS) throw new Error(`tab limit reached (${MAX_TABS})`);
    const {view,ready} = createView(size);
    const deadline = setTimeout(() => { if (!view.isDestroyed()) view.destroy(); }, 5000);
    try { await ready; } catch (error) { if (!view.isDestroyed()) view.destroy(); throw error; }
    finally { clearTimeout(deadline); }
    if (view.isDestroyed()) throw new Error("browser window closed during creation");
    // The initial blank document is already loaded. Navigating to it again
    // replaces its compositor surface just as a new viewer subscribes.
    if (url && url !== "about:blank") view.webContents.loadURL(url).catch(() => {});
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
      viewport: { ...entry.viewport },
    };
  }


  // ---- adapter 契约 ----

  return {
    dispose,
    // browser-tool 每次调用前设置（下载产物落盘目录）
    revision(tabId) { return revisions.get(tabId) || 0; },
    dialog(tabId) { return dialogs.get(tabId); },
    watchFrames(tabId, frame, closed) {
      const wc = tabs.get(Number(tabId))?.window.webContents;
      if (!wc || wc.isDestroyed()) throw new Error("Browser window closed");
      let watchers = frameWatches.get(wc.id);
      if (!watchers) frameWatches.set(wc.id, watchers = new Set());
      const watcher = {frame, closed};
      watchers.add(watcher);
      wc.invalidate();
      return () => { watchers.delete(watcher); if (!watchers.size) frameWatches.delete(wc.id); };
    },
    invalidateFrame(tabId) {
      const wc = tabs.get(Number(tabId))?.window.webContents;
      if (!wc || wc.isDestroyed()) throw new Error("Browser window closed");
      wc.invalidate();
    },
    inputFor(tabId) {
      return createBrowserInput(() => {
        const entry = tabs.get(Number(tabId));
        return entry ? {wc: entry.window.webContents, rect:{x:0,y:0,...entry.viewport,scale:1}} : null;
      });
    },
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
        void active; // Automation never changes frontend selection.
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
