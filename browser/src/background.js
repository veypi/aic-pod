/**
 * background.js — Service Worker entry point for AIC Browser Extension
 *
 * 1. Load settings from chrome.storage.local
 * 2. Initialize AICClient with credential + NATS URL
 * 3. Register web_browser tool
 * 4. Connect to NATS + publish CAPS
 * 5. Handle connect/disconnect based on settings
 */

import { AICClient } from "./sdk/client.js";
import { loadSettings } from "./sdk/storage.js";
import { webBrowserHandler } from "./tools/web_browser.js";

let client = null;
let connecting = false;

// ---- Tool Definition (mirrors agent-browser CLI) ----

const WEB_BROWSER_DEF = {
  name: "web_browser",
  description: "Control a web browser via the browser extension. Supported actions:\n" +
    "  open <url>                    Navigate to URL. Args: [\"https://example.com\"].\n" +
    "  click <sel>                   Click element by CSS selector or @ref. Args: [\"@e2\"] or [\"button.submit\"].\n" +
    "  close                         Close current active tab. No args.\n" +
    "  dblclick <sel>                Double-click element. Args: [\"@e3\"].\n" +
    "  download <sel> <path>         Click element to trigger download. path is relative filename. Args: [\"@e5\", \"report.xlsx\"].\n" +
    "  eval <js> [-b]                 Execute JavaScript in browser. Args: [\"document.title\"]. Use -b flag for base64-encoded JS: [\"-b\", \"<base64>\"]. Result returned as text.\n" +
    "  get <what> [selector]          Get page info. Args: [\"text\"], [\"html\"], [\"title\"], [\"url\"], [\"value\", \"@e1\"], [\"attr\", \"href\", \"@e5\"], [\"count\", \"a\"], [\"box\", \"@e1\"], [\"styles\", \"@e1\"], [\"cdp-url\"].\n" +
    "  network [id|--filter ...]      Inspect network requests. Args empty or flags: [\"--filter\", \"api\"], [\"--clear\"], [\"--type\", \"xhr,fetch\"]. First arg non-flag: [\"req-123\"] shows request detail.\n" +
    "  read [url]                     Fetch agent-readable page text. Args: [\"https://example.com\"] or omit for current page.\n" +
    "  screenshot [options]           Take JPEG screenshot. Returns image_data in attrs. Args: [] for visible area, [\"--full\"] for full page, [\"--quality\", \"40\"] to adjust quality (default 60, max 60).\n" +
    "  snapshot [options]             Accessibility tree with @ref labels. Args: [\"-i\"] interactive only, [\"-c\"] compact, [\"-d\", \"3\"] depth limit, [\"-s\", \"main\"] scope.\n" +
    "  tab <new|list|close|<n>>       Manage tabs. Args: [\"new\"], [\"list\"], [\"close\"], [\"0\"] switch to tab 0.\n" +
    "  wait <sel|ms|options>          Wait for condition. Args: [\"@e1\"] wait selector, [\"2000\"] wait ms, [\"--url\", \"**/dashboard\"], [\"--load\", \"networkidle\"], [\"--fn\", \"window.ready\"], [\"--text\", \"Welcome\"].\n" +
    "  scroll <dir> [px]              Scroll page. Args: [\"down\"], [\"up\", \"300\"].\n" +
    "  hover <sel>                    Hover element. Args: [\"@e1\"].\n" +
    "  fill <sel> <text>              Clear and fill input. Args: [\"@e5\", \"hello@example.com\"].\n" +
    "  press <key>                    Press key. Args: [\"Enter\"], [\"Control+a\"] for shortcuts.\n" +
    "  select <sel> <value>           Select dropdown option by value or text. Args: [\"#country\", \"CN\"].\n" +
    "  back                           Navigate back. No args.\n" +
    "  forward                        Navigate forward. No args.\n" +
    "  reload                         Reload page. No args.\n" +
    "  sleep <duration>               Pause. Args: [\"1s\"], [\"500ms\"], [\"2s\"].\n" +
    "  cookies <get|set|clear> [args] Manage cookies. Args: [\"get\"], [\"set\", \"--name\", \"token\", \"--value\", \"abc\"], [\"clear\", \"--name\", \"session\"].\n" +
    "  storage <local|session> <get|set|del> [args]  Web Storage. Args: [\"local\", \"get\", \"--key\", \"theme\"], [\"session\", \"set\", \"--key\", \"step\", \"--value\", \"1\"].\n" +
    "Use @ref labels from snapshot output to target elements in click, fill, hover, dblclick, select.",
  parameters: {
    type: "object",
    properties: {
      action: { type: "string", description: "Browser action to perform" },
      argv: { type: "array", items: { type: "string" }, description: "Per-action arguments" },
    },
    required: ["action", "argv"],
  },
  requiredLevel: 1,
  policyVersion: "1",
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
    version: "0.1.0",
    onLog: (fmt, ...args) => console.log(`[aic-browser] ${fmt}`, ...args),
  });

  client.registerTool(WEB_BROWSER_DEF, webBrowserHandler);

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
