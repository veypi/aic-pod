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
import { runVCmd } from "./sdk/vcmd.js";

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
      return { data: "" };
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
// fs.actions = [read/write/edit]（§4.5：与 page 端同一套 PageFS 代码，扩展 IndexedDB 后端，
//   按 host_id 寻址；browser screenshot 产出物落 $SESSION/screenshot/，fs.read 读图，§2.2）。

const BROWSER_LEVEL = 2;

// ---- 核心虚拟指令（vcmd，§5.4：与 page 端同一份 vcmd.js，操作扩展 PageFS）----
// 分级与 Go sdk/vcore/levels.go execCoreLevels 对齐：ls/rg/tree=Read(1)，rm/curl=Write(2)。
// desc/help 与 Go sdk/vcore/meta.go 同源；curl 无 cloud SSRF 行（扩展 fetch 无该限制）。
const VCMD_DECLS = [
  {
    name: "ls",
    level: 1,
    desc: "list directory entries",
    help: `ls [-l] [-a] [-t] [-h] [path]
  list directory entries, sorted by name (UTF-8 byte order)
  -l     include size and mtime (unix seconds) columns
  -a     include entries starting with '.' (hidden by default)
  -t     sort by mtime, newest first (ties by name)
  -h     human-readable size (1024-based, with -l)
  path   defaults to workdir`,
  },
  {
    name: "rg",
    level: 1,
    desc: "search content or list files",
    help: `rg <pattern> <path> | rg --files [-g <glob>]... [<path>]
  content search: recursive; output {path}:{line}:{content}
  -i        case-insensitive
  -l        print only matching file paths
  -m N      per-file match limit
  -n        print line numbers (default behavior, accepted for compatibility)
  -c        print only match count per file
  -w        match whole words only
  -g <glob> filename glob (basename match, repeatable, OR semantics)
  --files   list files recursively instead of searching
  --hidden  include hidden files and directories (excluded by default, like real rg)
  regex: RE2/Rust regex semantics (no lookaround/backreference);
         use bash -c "grep -P ..." on a physical host for PCRE`,
  },
  {
    name: "tree",
    level: 1,
    desc: "print directory tree (JSON)",
    help: `tree [path] [-L N]
  structured recursive directory tree, JSON output (always structured; no --json flag)
  path   defaults to workdir
  -L N   max depth (native tree semantics; --depth N accepted as alias), default 3, max 5;
         node cap 2000 (truncated=true)
  hidden items (.xxx) excluded entirely (GNU tree default; -a not supported);
  node_modules/vendor/__pycache__/dist/build/.next etc. listed as leaves without recursion`,
  },
  {
    name: "rm",
    level: 2,
    desc: "remove files or directories",
    help: `rm [-r] <path>
  remove a file or empty directory; -r for recursive delete
  root directories are hard-protected (cannot be removed)`,
  },
  {
    name: "curl",
    level: 2,
    desc: "download a URL to a file",
    help: `curl -o <path> <url> [--max-size <MB>]
  HTTP(S) GET, streamed to <path>; target must not exist
  --max-size default 1024MB, cap 10240MB (aborts and cleans partial file)`,
  },
];

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

  // 核心虚拟指令（vcmd，§5.4）：ls/rg/tree/rm/curl 与 page 端同构，
  // 操作扩展 PageFS（扩展 origin IndexedDB），handler 统一走 runVCmd。
  for (const d of VCMD_DECLS) {
    client.registerCommand(d.name, d.level, async (ctx, data) => {
      const out = await runVCmd(d.name, data.argv || [], ctx.fs);
      return { state: "completed", content: out.content, attrs: out.attrs };
    }, { desc: d.desc, help: d.help });
  }

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
