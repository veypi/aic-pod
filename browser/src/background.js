/**
 * background.js — Service Worker entry point for AIC Browser Extension
 *
 * 1. Load settings from chrome.storage.local
 * 2. Initialize AICClient with credential + NATS URL
 * 3. Register browser tool
 * 4. Connect to NATS + publish CAPS
 * 5. Handle connect/disconnect based on settings
 */

import { AICClient } from "./sdk/client.js";
import { loadSettings } from "./sdk/storage.js";
import { browserHandler } from "./tools/browser.js";

let client = null;
let connecting = false;

// ---- Tool Definition (mirrors agent-browser CLI) ----

const BROWSER_DEF = {
  name: "browser",
  description: "Control the user's real browser (Chrome extension host). " +
    "Core actions: open, click, close, download, eval, get, network, read, screenshot, snapshot, tab, wait, sleep. " +
    "Refs (@eN) from snapshot become stale when the page changes — always re-snapshot before the next ref interaction. " +
    "Extended actions (save/upload/pipeline/var) are not implemented on this host.",
  parameters: {
    type: "object",
    properties: {
      action: { type: "string", description: "Browser action to perform" },
      argv: { type: "array", items: { type: "string" }, description: "Per-action arguments" },
    },
    required: ["action", "argv"],
  },
  actions: ["open", "click", "close", "download", "eval", "get", "network", "read", "screenshot", "snapshot", "tab", "wait", "sleep"],
  requiredLevel: 2,
  policyVersion: "2",
};

// ---- Lifecycle ----

async function start() {
  const settings = await loadSettings();

  if (!settings.key || !settings.key.trim()) {
    console.log("[aic-browser] No credential configured. Open extension options to set up.");
    return;
  }

  if (!settings.autoConnect) {
    console.log("[aic-browser] autoConnect disabled. Skipping connection.");
    return;
  }

  await connect(settings);
}

async function connect(settings) {
  if (connecting) {
    console.log("[aic-browser] Connection already in progress, skipping");
    return;
  }

  // Close previous client
  if (client) {
    await client.close();
    client = null;
  }

  connecting = true;
  setBadge(false);

  client = new AICClient({
    key: settings.key,
    url: settings.url || "wss://ivec.ai/aic/api/nc",
    deviceName: settings.deviceName || "Chrome",
    deviceType: "browser",
    // 版本以 manifest.json 为单一来源，上报格式 va.b.c（服务端主版本门禁）
    version: "v" + chrome.runtime.getManifest().version,
    onLog: (fmt, ...args) => console.log(`[aic-browser] ${fmt}`, ...args),
  });

  client.registerTool(BROWSER_DEF, browserHandler);

  try {
    await client.connect();
    console.log("[aic-browser] Connected successfully");
    setBadge(true);
    connecting = false;
  } catch (err) {
    console.error("[aic-browser] Connection failed:", err.message);
    setBadge(false);
    connecting = false;
    // Retry after 10s
    setTimeout(() => {
      const retry = async () => {
        const s = await loadSettings();
        if (s.autoConnect !== false) await connect(s);
      };
      retry();
    }, 10_000);
  }
}

async function disconnect() {
  connecting = false;
  if (client) {
    await client.close();
    client = null;
    setBadge(false);
    console.log("[aic-browser] Disconnected");
  }
}

// ---- Badge ----

function setBadge(connected) {
  if (connected) {
    chrome.action.setBadgeText({ text: "ON" });
    chrome.action.setBadgeBackgroundColor({ color: "#22c55e" });
  } else {
    chrome.action.setBadgeText({ text: "" });
  }
}

// ---- Message handlers (from options page) ----

chrome.runtime.onMessage.addListener((msg, sender, sendResponse) => {
  (async () => {
    switch (msg.type) {
      case "connect":
        const settings = await loadSettings();
        await connect(settings);
        sendResponse({ success: true });
        break;

      case "disconnect":
        await disconnect();
        sendResponse({ success: true });
        break;

      case "status":
        sendResponse({
          connected: client !== null && !client.closed,
          envID: client ? client.envID : null,
        });
        break;

      default:
        sendResponse({ error: "unknown message type" });
    }
  })();
  return true; // keep channel open for async response
});

// ---- Startup ----

start();

// Handle extension install/update
chrome.runtime.onInstalled.addListener(() => {
  console.log("[aic-browser] Extension installed/updated");
  // Open options page on install if no credential
  loadSettings().then(s => {
    if (!s.key) {
      chrome.runtime.openOptionsPage();
    }
  });
});
