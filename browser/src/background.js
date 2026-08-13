/**
 * background.js — Service Worker entry point for AIC Browser Extension
 *
 * 1. Load settings from chrome.storage.local
 * 2. Initialize AICClient with credential + NATS URL
 * 3. Register browser tool
 * 4. Connect to NATS + publish CAPS
 * 5. Handle connect/disconnect based on settings
 */

import { AICClient, platformURL } from "./sdk/client.js";
import { loadSettings, saveSettings } from "./sdk/storage.js";
import { browserHandler } from "./tools/browser.js";

// ---- 本地通道（平台 /settings/local 页面经 content script 桥接调用）----
// localCode 会话级随机码，生命周期 = 浏览器进程（等同桌面端 local_code 语义）。
// 格式 "ext.{hex}"：与桌面端 "{port}.{code}" 同串解析，port 段非数字即插件通道。
const localCode = "ext." + crypto.randomUUID().replaceAll("-", "");

// handleLocalCall 是 /settings/local 页面的本地 API 子集（少于桌面端：
// 无 work_dir/exec_timeout/get_log）。所有调用须经 code 校验。
async function handleLocalCall(msg) {
  if (msg.code !== localCode) {
    return { error: "invalid local code" };
  }
  const settings = await loadSettings();
  switch (msg.method) {
    case "get_config":
      return { data: { host: platformURL(settings.host) } };
    case "set_config":
      // 白名单为空：运行参数与设备名均不由插件管理（改名走平台 host.name）
      return { data: { ok: true } };
    case "bind": {
      const credential = String(msg.args?.credential || "").trim();
      if (!credential) return { error: "credential is empty" };
      settings.key = credential;
      await saveSettings(settings);
      try {
        await connect(settings);
      } catch (err) {
        return { error: err?.message || String(err) };
      }
      return { data: { ok: true, host: platformURL(settings.host) } };
    }
    case "unbind":
      await disconnect();
      settings.key = "";
      await saveSettings(settings);
      return { data: { ok: true } };
    case "get_status": {
      // host_id 从存储凭证解析（与连接状态无关，断连/未启动也能识别绑定设备）
      const parts = (settings.key || "").split(".");
      return {
        data: {
          running: client !== null && client.connected === true,
          host_id: parts.length === 4 ? parts[0] : "",
          hostname: "Chrome",
          version: "v" + chrome.runtime.getManifest().version,
        },
      };
    }
    case "get_log":
      return { data: { log: "" } };
    case "start":
      try {
        await connect(await loadSettings());
      } catch (err) {
        return { error: err?.message || String(err) };
      }
      return { data: { ok: true } };
    case "stop":
      await disconnect();
      return { data: { ok: true } };
    default:
      return { error: "unknown local method: " + msg.method };
  }
}

let client = null;
let connectPromise = null; // 进行中的连接 promise（重复调用合并 await，不误报）

// 首连超时（nats-core wsconnect 对不可达地址无限重连、永不 resolve——
// 必须在扩展层兜底：超时报错而非 UI 误报已连接）。
const CONNECT_TIMEOUT_MS = 15000;

// ---- 能力声明（指令集 v2，caps 由 client 汇总发布） ----
// exec.commands 统一命令表 = client 恒声明 commands（§5.1，能力发现，服务端
// 对 host commands 走应答式转发（§5.2，与 page 同构））+ 下方注册命令：browser（分级与 Go
// vcore meta.go 的 browser 条目一致）：
//   required_level = 2（Write：局部、可逆、低爆炸半径）
//   stateful = true（同 (session, host) 串行，服务端 slocks 保证）
//   backgroundable = true（download/wait 长操作可后台化）
// fs.actions = [read/write/edit/ls/rg/cp/mv/rm]（§4：与 page 端同一套 PageFS+fsops 代码，
//   扩展 IndexedDB 后端，按 host_id 寻址；browser screenshot 产出物落 /screenshot/，fs.read 读图，§2.2）。

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

function connect(settings) {
  // 合并进行中的连接：重复调用（autoConnect 已挂起 + 手动点击）await 同一个
  // promise——此前 `if (connecting) return` 导致第二次调用立即成功返回，
  // UI 误报"已连接"。
  if (connectPromise) return connectPromise;
  connectPromise = (async () => {
    // Close previous client
    if (client) {
      await client.close();
      client = null;
    }
    setBadge(false);

    const c = new AICClient({
      key: settings.key,
      host: settings.host || "https://ivec.ai",
            deviceType: "browser",
      // 版本以 manifest.json 为单一来源，上报格式 va.b.c（服务端主版本门禁）
      version: "v" + chrome.runtime.getManifest().version,
      onLog: (fmt, ...args) => console.log(`[aic-browser] ${fmt}`, ...args),
    });
    client = c;

  client.registerCommand("browser", BROWSER_LEVEL, browserHandler, {
    // desc/help 与 Go libs/vcore/meta.go 的 browser 条目同源（命令语义以 agent-browser 为基准）
    desc: "control a web browser (agent-browser CLI)",
    help: `browser <subcommand> [args...] — browser automation (AIC Browser Extension, v2)

Supported (13):
  open <url>                  Navigate to URL in the AI workspace tab (background/incognito mode) or active tab (coop mode); http/https only
  read [url]                  Extract readable page text (no url = current page)
  click <sel|@ref>            Click element (CSS selector or @ref from snapshot)
  eval <js>                   Run JS via CDP (CSP-independent, DevTools semantics; use for type/fill/navigation workarounds)
  get <what> [sel]            text / html / title / url / value / attr <sel> <name> / count / box / styles
  network [id|requests] [--filter s] [--type t] [--method m] [--status n] [--limit n] [--clear]
                              List / detail network requests (id or "requests" = list)
  screenshot [--quality N]    Save JPEG to this host's fs (/screenshot/); read it with fs read on this host
  snapshot [-i] [-c] [-d N] [-s sel]  Accessibility tree with @refs (stale refs require re-snapshot)
  tab <new|list|close|N>      Manage tabs inside the AI workspace (never activates/steals focus)
  wait <sel|ms> [--url g] [--load l] [--fn js] [--text t] [--download f]
                              Wait for selector/ms/condition (default 30s)
  download <sel> <path>       Click element to trigger a download (waits 30s)
  close                       Close the AI workspace tab (coop mode: active tab)
  sleep <dur>                 Sleep (e.g. 1s, 500ms)

Unsupported (agent-browser CLI only; calling them returns unknown action):
  type fill press keyboard hover focus check uncheck select drag dblclick
  scroll scrollintoview pdf back forward reload is find mouse set cookies
  storage diff trace profiler record console errors highlight inspect
  clipboard batch auth session connect pushstate mcp skills upload
  → 输入/导航/滚动等缺失指令用 eval <js> 替代（如 el.value=...; el.dispatchEvent(new Event('input'))）

Behavior:
  stateful: serialized per (session, host) — click/snapshot races corrupt @refs
  工作区：background/incognito 模式所有操作在 AI 专属标签页/无痕窗口，不激活不抢焦点；
  协作模式（两开关都关）操作当前激活标签页
  close 后下一条指令自动重建工作区标签页（cookies/storage 保留）`,
    stateful: true, // 内部实现细节（串行链），不进协议
    backgroundable: true, // 内部实现细节（后台化），不进协议
  });

  // 文件类指令已迁入 fs 通道（8 action，PageFS+fsops）：exec 侧只注册 browser
  //（恒声明 commands 由 client 自带）。

    try {
      await c.connect();
      console.log("[aic-browser] Connected successfully");
      setBadge(true);
    } catch (err) {
      console.error("[aic-browser] Connection failed:", err.message);
      setBadge(false);
      await c.close();
      // 自动重试（仅 autoConnect 场景；配置错误时用户改地址后手动连接即恢复）
      const s = await loadSettings();
      if (s.autoConnect !== false) {
        setTimeout(() => {
          loadSettings().then((s2) => {
            if (s2.autoConnect !== false) connect(s2);
          });
        }, 10_000);
      }
      throw err; // 传播给调用方（message handler 回传 error，UI 显示失败原因）
    } finally {
      connectPromise = null;
    }
  })();
  return connectPromise;
}

async function disconnect() {
  connectPromise = null; // 取消合并（进行中的连接自行收尾，finally 幂等置空）
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
        try {
          const settings = await loadSettings();
          await connect(settings);
          sendResponse({ success: true });
        } catch (err) {
          sendResponse({ success: false, error: err?.message || String(err) });
        }
        break;

      case "disconnect":
        await disconnect();
        sendResponse({ success: true });
        break;

      case "status":
        // 真实连接判定：client 存在且 NATS 连接建立过（非 new 即真）
        sendResponse({
          connected: client !== null && client.connected === true,
          hostID: client ? client.hostID : null,
        });
        break;

      case "history":
        sendResponse({ history: client ? await client.historySnapshot() : [] });
        break;

      case "history:clear":
        if (client) await client.historyClear();
        sendResponse({ success: true });
        break;

      // popup 获取 /settings/local 链接（携带本次会话 localCode）
      case "local-link": {
        const settings = await loadSettings();
        sendResponse({ url: `${platformURL(settings.host)}/hosts?local_code=${localCode}` });
        break;
      }

      // 平台 /settings/local 页面经 content script 桥接的本地调用
      case "__aic_local":
        sendResponse(await handleLocalCall(msg));
        break;

      default:
        sendResponse({ error: "unknown message type" });
    }
  })();
  return true; // keep channel open for async response
});

// ---- SW 保活与自愈重连（2026-08-06）----
// MV3：SW 空闲 ~30s 被 Chrome 回收，NATS WebSocket 随之断开，且没有任何事件能唤醒
// 已死的 SW（平台指令要经已断开的 WS 才能到达）→ 全部请求超时，直到用户手动碰扩展。
// alarms 由浏览器托管：SW 死后仍会按时触发并唤醒它——每 30s 巡检连接状态，断开即重连。
const KEEPALIVE_ALARM = "aic-keepalive";

chrome.alarms.create(KEEPALIVE_ALARM, { periodInMinutes: 0.5 });
chrome.alarms.onAlarm.addListener(async (alarm) => {
  if (alarm.name !== KEEPALIVE_ALARM) return;
  const s = await loadSettings();
  if (!s.key || !s.key.trim() || s.autoConnect === false) return;
  const alive = client !== null && client.nc && !client.nc.isClosed();
  if (!alive) {
    console.log("[aic-browser] keepalive: SW restarted or NATS disconnected, reconnecting");
    try {
      await connect(s);
    } catch (err) {
      console.log("[aic-browser] keepalive reconnect failed:", err?.message || err);
    }
  }
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
