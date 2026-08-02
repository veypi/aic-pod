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

// ---- 能力声明（指令集 v2，caps 由 client 汇总发布） ----
// exec 虚拟指令：browser（核心 13 + 扩展集未实现）。分级与 Go vcore/browser.VirtualDecl 一致：
//   required_level = 2（Write：局部、可逆、低爆炸半径）
//   stateful = true（同 (session, host) 串行，服务端 slocks 保证）
//   backgroundable = true（download/wait 长操作可后台化）
// fs.actions = [read/write/edit]（§4.5：与 page 端同一套 PageFS 代码，扩展 IndexedDB 后端，
//   按 host_id 寻址；browser screenshot 产出物落 $SESSION/screenshot/，fs.read 读图，§2.2）；
// exec.programs = []（显式纯虚拟）。

const BROWSER_LEVEL = 2;

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

  client.registerVirtual("browser", BROWSER_LEVEL, browserHandler, {
    // desc/help 与 Go sdk/vcore/meta.go 的 browser 条目同源（命令语义以 agent-browser 为基准）
    desc: "control a web browser (agent-browser CLI)",
    help: `browser <subcommand> [args...] — fast browser automation for AI agents

Start here:
  browser snapshot             Accessibility tree with @refs (for AI)
  browser snapshot -i          Interactive elements only
  Every element gets a @ref; other actions target it with @<ref>:
  browser click @e2            Click by ref from snapshot

Core Commands:
  open <url>                   Navigate to URL (http/https only)
  read [url]                   Fetch agent-readable text (NOTE: navigates the page)
  click <sel> / dblclick <sel> Click / double-click element (or @ref)
  type <sel> <text>            Type into element
  fill <sel> <text>            Clear and fill
  press <key>                  Press key (Enter, Tab, Control+a)
  keyboard type <text>         Type with real keystrokes (no selector)
  hover <sel> / focus <sel>    Hover / focus element
  check <sel> / uncheck <sel>  Check / uncheck checkbox
  select <sel> <val...>        Select dropdown option
  drag <src> <dst>             Drag and drop
  upload <sel> <files...>      Upload files (cloud: sources must be inside $SESSION/$USER/$AGENT)
  download <sel> <path>        Download file by clicking element (cloud: path inside $SESSION)
  scroll <dir> [px]            Scroll (up/down/left/right)
  scrollintoview <sel>         Scroll element into view
  wait <sel|ms>                Wait for element or time
  screenshot [--full]          Take screenshot; saved to this host's fs ($SESSION/screenshot/, image is NOT returned — use fs read on this host to view it)
  pdf <path>                   Save page as PDF
  snapshot [-i] [-c] [-d N] [-s sel]  Accessibility tree with refs
  eval <js>                    Run JavaScript (awaits promises)
  close [--all]                Close browser (--all closes every session)

Navigation:  back / forward / reload

Get Info:  browser get <what> [selector]
  text, html, value, title, url, count, box, styles (selector required except title/url/value)
  attr:  browser get attr <selector> <name>

Check State:  browser is <what> <selector>   visible / enabled / checked

Find Elements:  browser find <locator> <value> <action> [text]
  role, text, label, placeholder, alt, title, testid, first, last, nth

Mouse:  browser mouse <action> [args]   move <x> <y>, down [btn], up [btn], wheel <dy> [dx]

Browser Settings:  browser set <setting> [value]
  viewport <w> <h>, device <name>, geo <lat> <lng>, offline [on|off], media [dark|light]

Network:  browser network <sub> [args]
  route <url> [--abort|--body <json>]  Intercept/mock requests
  unroute [url]                        Remove route
  requests [--clear] [--filter p] [--type t] [--method m] [--status c]   List requests
  request <id>                         Request detail (with body)
  har <start|stop> [path]              Record/export HAR

Storage:
  cookies [get|set|clear]      Manage cookies
  storage <local|session>      Manage web storage

Tabs:  browser tab [new|list|close|<n>]   Manage tabs (monotonic ids: t1, t2, ...)

Diff:  browser diff snapshot | diff screenshot --baseline | diff url <u1> <u2>

Debug:
  trace start | trace stop [path]       Chrome DevTools trace
  profiler start|stop [path]            Chrome DevTools profile
  record start <path> [url] | record stop  Video recording (WebM)
  console [--clear] / errors [--clear]  Console logs / page errors
  highlight <sel> / inspect / clipboard <op> [text]

Batch:  browser batch [--bail] ["cmd" ...]   Execute multiple commands sequentially

Auth Vault:  browser auth save <name> | auth login <name> | auth list | auth show <name> | auth delete <name>

Sessions:  browser session | session list

Others:  browser connect <port|url> | pushstate <url> | mcp | skills get core

Behavior:
  stateful: serialized per (session, host) — click/snapshot races corrupt @refs
  download/wait can background: returns background=true + id, then bg_wait/bg_kill
  close auto-restarts: next command launches a fresh browser (cookies/storage reset)`,
    stateful: true, // 内部实现细节（串行链），不进协议
    backgroundable: true, // 内部实现细节（后台化），不进协议
  });

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
          hostID: client ? client.hostID : null,
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
