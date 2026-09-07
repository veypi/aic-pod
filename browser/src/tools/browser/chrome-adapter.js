/**
 * chrome-adapter.js — browser core 的 Chrome 插件宿主适配器
 *
 * chrome.* API 映射（core.js adapter 契约）：
 *   tabs     → chrome.tabs（query/get/create/update/remove/onUpdated）
 *   windows  → chrome.windows（normal 窗口枚举/incognito 创建）
 *   evalIn   → chrome.scripting.executeScript（MAIN 世界，func+args 结构化序列化，
 *              不受页面 CSP 动态执行限制）
 *   cdp      → chrome.debugger（短 attach/detach；attach 返回值串语义归一为 throw）
 *   downloads→ chrome.downloads（onCreated 一次性监听 / search 完成态）
 *   store    → chrome.storage.local（MV3 SW 休眠/重启后工作区状态恢复）
 *   settings → sdk/storage.js loadSettings（incognito/background 开关）
 */

import { loadSettings } from "../../sdk/storage.js";

const CDP_PROTOCOL = "1.3";

export function createChromeAdapter() {
  return {
    tabs: {
      async active() {
        const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
        if (!tab) throw new Error("no active tab");
        return tab;
      },
      get(id) {
        return chrome.tabs.get(id);
      },
      // windowId 省略 = 当前窗口（协作模式）
      list(windowId) {
        return windowId === undefined
          ? chrome.tabs.query({ currentWindow: true })
          : chrome.tabs.query({ windowId });
      },
      create(opts) {
        return chrome.tabs.create(opts);
      },
      update(id, props) {
        return chrome.tabs.update(id, props);
      },
      async remove(id) {
        await chrome.tabs.remove(id);
      },
      onUpdated: {
        add(fn) {
          chrome.tabs.onUpdated.addListener(fn);
        },
        remove(fn) {
          chrome.tabs.onUpdated.removeListener(fn);
        },
      },
    },

    windows: {
      async listNormal() {
        return chrome.windows.getAll({ windowTypes: ["normal"] });
      },
      get(id) {
        return chrome.windows.get(id);
      },
      async createIncognitoUnfocused() {
        try {
          return await chrome.windows.create({ incognito: true, focused: false });
        } catch (e) {
          throw new Error(`incognito window create failed: enable the extension in incognito (chrome://extensions → AIC Browser → Allow in Incognito): ${e?.message || e}`);
        }
      },
    },

    async evalIn(tabId, func, args) {
      const results = await chrome.scripting.executeScript({
        target: { tabId },
        func,
        args: args || [],
      });
      if (!results || results.length === 0) throw new Error("script execution failed");
      return results[0].result;
    },

    cdp: {
      async attach(tabId) {
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
      },
      send(tabId, method, params) {
        return chrome.debugger.sendCommand({ tabId }, method, params);
      },
      async detach(tabId) {
        await chrome.debugger.detach({ tabId });
      },
    },

    downloads: {
      onceCreated(fn) {
        const listener = (item) => {
          chrome.downloads.onCreated.removeListener(listener);
          fn(item);
        };
        chrome.downloads.onCreated.addListener(listener);
        return () => chrome.downloads.onCreated.removeListener(listener);
      },
      searchComplete(escapedRegex) {
        return chrome.downloads.search({
          filenameRegex: escapedRegex,
          state: "complete",
        });
      },
    },

    store: {
      async get(key) {
        const r = await chrome.storage.local.get(key);
        return r[key] || null;
      },
      async set(key, value) {
        await chrome.storage.local.set({ [key]: value ?? null });
      },
    },

    settings() {
      return loadSettings();
    },
  };
}
