/**
 * web_browser.js — Browser automation tool for Chrome Extension
 *
 * Action signatures match agent-browser CLI.
 * Implementation uses Chrome Extension APIs (tabs, scripting, cookies, etc.).
 */

// ---- main entry ----

export async function webBrowserHandler(ctx, toolData) {
  const params = parseParams(toolData);
  if (!params) {
    return { status: "error", error: "invalid tool_data: expected {action, argv}" };
  }

  const { action, argv } = params;
  const timeout = parseTimeout(argv) || 30_000;

  try {
    const result = await executeAction(action, argv, timeout);
    result.attrs = { ...(result.attrs || {}), action };
    // Add tab info
    try {
      const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
      if (tab) {
        result.attrs.tab_id = String(tab.id);
        result.attrs.url = tab.url || "";
      }
    } catch (_) { /* ignore */ }
    return result;
  } catch (err) {
    return { status: "error", error: err.message, attrs: { action } };
  }
}

// ---- param parsing ----

function parseParams(data) {
  if (!data) return null;
  // data might already be an object, or a JSON string
  if (typeof data === "object" && data.action !== undefined) {
    return { action: String(data.action || "").trim(), argv: parseArgv(data.argv) };
  }
  return null;
}

function parseArgv(argv) {
  if (!argv) return [];
  if (Array.isArray(argv)) return argv.map(String);
  return [];
}

function parseTimeout(argv) {
  const idx = argv.indexOf("--timeout");
  if (idx >= 0 && idx + 1 < argv.length) {
    const v = parseInt(argv[idx + 1], 10);
    return isNaN(v) ? null : v;
  }
  return null;
}

function parseFlags(argv, flagMap) {
  const result = {};
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (flagMap[a] !== undefined) {
      if (flagMap[a] === "flag") {
        result[a] = true;
      } else if (i + 1 < argv.length && !argv[i + 1].startsWith("-")) {
        result[a] = argv[i + 1];
        i++;
      }
    }
  }
  return result;
}

// ---- action dispatcher ----

async function executeAction(action, argv, timeout) {
  switch (action) {
    case "open":      return open(argv);
    case "click":     return click(argv, timeout);
    case "close":     return closeTab();
    case "dblclick":  return dblclick(argv, timeout);
    case "download":  return download(argv, timeout);
    case "eval":      return evaluate(argv, timeout);
    case "get":       return get(argv, timeout);
    case "network":   return network(argv);
    case "read":      return read(argv, timeout);
    case "screenshot":return screenshot(argv);
    case "snapshot":  return snapshot(argv, timeout);
    case "tab":       return tab(argv);
    case "wait":      return wait(argv, timeout);
    case "scroll":    return scroll(argv, timeout);
    case "hover":     return hover(argv, timeout);
    case "fill":      return fill(argv, timeout);
    case "press":     return press(argv, timeout);
    case "select":    return selectDropdown(argv, timeout);
    case "back":      return navigation("back");
    case "forward":   return navigation("forward");
    case "reload":    return navigation("reload");
    case "sleep":     return sleep(argv);
    case "cookies":   return cookies(argv);
    case "storage":   return storage(argv, timeout);
    // TODO: pipeline not yet implemented
    // case "pipeline":  return pipeline(argv, timeout);
    default:
      return { status: "error", error: `unsupported action: "${action}"` };
  }
}

// ---- download helper ----

async function downloadFile(dataUrl, filename) {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error("download timeout")), 15_000);
    let downloadId = null;

    const listener = (delta) => {
      if (delta.id !== downloadId) return;
      if (delta.state?.current === "complete") {
        clearTimeout(timer);
        chrome.downloads.onChanged.removeListener(listener);
        // Get absolute local path
        chrome.downloads.search({ id: downloadId }, (results) => {
          if (results.length && results[0].filename) {
            resolve(results[0].filename);
          } else {
            resolve(filename);
          }
        });
      }
      if (delta.state?.current === "interrupted") {
        clearTimeout(timer);
        chrome.downloads.onChanged.removeListener(listener);
        reject(new Error("download interrupted"));
      }
    };
    chrome.downloads.onChanged.addListener(listener);

    chrome.downloads.download({ url: dataUrl, filename, saveAs: false }, (id) => {
      if (chrome.runtime.lastError) {
        clearTimeout(timer);
        chrome.downloads.onChanged.removeListener(listener);
        reject(new Error(chrome.runtime.lastError.message));
        return;
      }
      downloadId = id;
    });
  });
}

// ---- helpers ----

async function getActiveTab() {
  // Try current window active tab first
  let [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
  if (tab) return tab;
  // Fallback: any tab in any window
  [tab] = await chrome.tabs.query({});
  if (tab) return tab;
  throw new Error("no active tab");
}

async function execInTab(tabId, func, args) {
  try {
    const results = await chrome.scripting.executeScript({
      target: { tabId },
      func,
      args: args || [],
      world: "MAIN",
    });
    return results[0]?.result;
  } catch (err) {
    throw new Error(`script execution failed: ${err.message}`);
  }
}

/**
 * Resolve a selector: @ref uses the a11y snapshot ref resolver,
 * otherwise treat as CSS selector.
 */
function resolveSelector(sel) {
  if (sel.startsWith("@")) {
    const ref = sel.slice(1);
    return `[data-aic-ref="${ref}"]`;
  }
  return sel;
}

/**
 * Inject a content script to set up @ref data attributes after snapshot.
 * Returns the target selector string.
 */
async function resolveRefSelector(tabId, sel) {
  if (!sel.startsWith("@")) return sel;
  const ref = sel.slice(1);
  return `[data-aic-ref="${ref}"]`;
}

// ---- action implementations ----

async function open(argv) {
  const url = argv[0];
  if (!url) return { status: "error", error: "open requires a URL" };
  if (!url.startsWith("http://") && !url.startsWith("https://")) {
    return { status: "error", error: `invalid URL: ${url}` };
  }

  const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
  if (tab && tab.id) {
    await chrome.tabs.update(tab.id, { url });
    // Wait for page to start loading
    await waitForLoad(tab.id, 3000);
  } else {
    await chrome.tabs.create({ url });
  }
  return { content: `Navigated to ${url}` };
}

async function waitForLoad(tabId, ms) {
  return new Promise(resolve => {
    const timeout = setTimeout(resolve, ms);
    const listener = (updatedTabId, info) => {
      if (updatedTabId === tabId && info.status === "complete") {
        clearTimeout(timeout);
        chrome.tabs.onUpdated.removeListener(listener);
        setTimeout(resolve, 500); // extra time for JS to settle
      }
    };
    chrome.tabs.onUpdated.addListener(listener);
  });
}

async function click(argv, timeout) {
  if (argv.length < 1) return { status: "error", error: "click requires a selector" };
  const tab = await getActiveTab();
  const sel = await resolveRefSelector(tab.id, argv[0]);
  return await execInTab(tab.id, clickElement, [sel, timeout]);
}

function clickElement(sel, timeout) {
  const el = document.querySelector(sel);
  if (!el) return { content: "", error: `element not found: ${sel}` };
  el.scrollIntoView({ block: "center" });
  el.click();
  return { content: `Clicked ${sel}` };
}

async function dblclick(argv, timeout) {
  if (argv.length < 1) return { status: "error", error: "dblclick requires a selector" };
  const tab = await getActiveTab();
  const sel = await resolveRefSelector(tab.id, argv[0]);
  const result = await execInTab(tab.id, (_sel) => {
    const el = document.querySelector(_sel);
    if (!el) return { error: `element not found: ${_sel}` };
    el.scrollIntoView({ block: "center" });
    el.dispatchEvent(new MouseEvent("dblclick", { bubbles: true }));
    return { content: `Double-clicked ${_sel}` };
  }, [sel]);
  if (result.error) return { status: "error", error: result.error };
  return result;
}

async function closeTab() {
  const tabs = await chrome.tabs.query({ currentWindow: true });
  if (tabs.length <= 1) {
    return { content: "Cannot close last tab" };
  }
  const [active] = await chrome.tabs.query({ active: true, currentWindow: true });
  if (active) {
    await chrome.tabs.remove(active.id);
  }
  return { content: "Tab closed" };
}

async function download(argv, timeout) {
  if (argv.length < 2) return { status: "error", error: "download requires selector and path" };
  const tab = await getActiveTab();
  const sel = await resolveRefSelector(tab.id, argv[0]);
  const path = argv[1];

  // Click the element to trigger download
  await execInTab(tab.id, (_sel) => {
    const el = document.querySelector(_sel);
    if (!el) throw new Error(`element not found: ${_sel}`);
    el.scrollIntoView({ block: "center" });
    el.click();
  }, [sel]);

  // Wait for download to complete
  return new Promise((resolve) => {
    const timer = setTimeout(() => {
      chrome.downloads.onChanged.removeListener(listener);
      resolve({ content: `Download started: ${path}` });
    }, timeout || 15000);

    const listener = (delta) => {
      if (delta.state?.current === "complete") {
        clearTimeout(timer);
        chrome.downloads.onChanged.removeListener(listener);
        resolve({ content: `Downloaded: ${path}`, attrs: { path } });
      }
    };
    chrome.downloads.onChanged.addListener(listener);
  });
}

async function evaluate(argv, timeout) {
  let js = argv[0];
  if (!js) return { status: "error", error: "eval requires JavaScript code" };

  const flags = parseFlags(argv, { "-b": "flag" });
  if (flags["-b"]) {
    js = decodeBase64(js);
  }

  const tab = await getActiveTab();
  const result = await execInTab(tab.id, (_js) => {
    try {
      const val = eval(_js);
      return { content: val === undefined ? "undefined" : String(val) };
    } catch (e) {
      return { error: e.message };
    }
  }, [js]);

  if (result.error) return { status: "error", error: result.error };
  return result;
}

function decodeBase64(s) {
  try {
    return atob(s);
  } catch {
    return s;
  }
}

async function get(argv, timeout) {
  const what = argv[0];
  if (!what) return { status: "error", error: "get requires a 'what' argument" };

  const tab = await getActiveTab();

  // Handle what's that don't need selector
  switch (what) {
    case "title":
      return { content: tab.title || "" };
    case "url":
      return { content: tab.url || "" };
    case "cdp-url": {
      // Get the debugging URL via CDP
      return { content: `chrome://inspect` };
    }
  }

  const sel = argv[1] ? await resolveRefSelector(tab.id, argv[1]) : null;

  const result = await execInTab(tab.id, (_what, _sel) => {
    const el = _sel ? document.querySelector(_sel) : document.body;
    if (!el && _sel) return { error: `element not found: ${_sel}` };

    switch (_what) {
      case "text":
        return { content: (el || document.body).innerText || "" };
      case "html":
        return { content: (el || document.documentElement).outerHTML || "" };
      case "value":
        return { content: el ? (el.value ?? "") : "" };
      case "attr": {
        // attr requires an extra arg for the attribute name
        return { content: el ? (el.getAttribute(arguments[2]) ?? "") : "" };
      }
      case "count":
        return { content: String(document.querySelectorAll(_sel || "body").length) };
      case "box": {
        if (!el) return { error: `element not found: ${_sel}` };
        const r = el.getBoundingClientRect();
        return { content: JSON.stringify({ x: r.x, y: r.y, width: r.width, height: r.height }) };
      }
      case "styles": {
        if (!el) return { error: `element not found: ${_sel}` };
        const cs = window.getComputedStyle(el);
        const styles = {};
        for (let i = 0; i < cs.length; i++) {
          const k = cs[i];
          styles[k] = cs.getPropertyValue(k);
        }
        return { content: JSON.stringify(styles) };
      }
      default:
        return { error: `unknown get what: ${_what}` };
    }
  }, [what, sel, argv[2] || ""]);

  if (result?.error) return { status: "error", error: result.error };
  return result;
}

// ---- network (fetch/XHR interceptor) ----

let networkInterceptorId = null;

async function network(argv) {
  const flags = parseFlags(argv, {
    "--filter": "string", "--type": "string",
    "--method": "string", "--status": "string",
    "--clear": "flag",
  });

  if (flags["--clear"]) {
    return await clearNetworkLogs();
  }

  const reqId = argv[0] && !argv[0].startsWith("--") ? argv[0] : null;

  if (reqId) {
    return await getNetworkRequest(reqId);
  }

  return await listNetworkRequests(flags);
}

async function listNetworkRequests(flags) {
  const tab = await getActiveTab();
  const result = await execInTab(tab.id, (_flags) => {
    const logs = window.__aic_network_logs || [];
    let filtered = logs;
    if (_flags["--filter"]) {
      const f = _flags["--filter"].toLowerCase();
      filtered = filtered.filter(r => r.url.toLowerCase().includes(f));
    }
    if (_flags["--type"]) {
      const types = _flags["--type"].split(",");
      filtered = filtered.filter(r => types.includes(r.type));
    }
    if (_flags["--method"]) {
      filtered = filtered.filter(r => r.method === _flags["--method"].toUpperCase());
    }
    if (_flags["--status"]) {
      const s = parseInt(_flags["--status"], 10);
      filtered = filtered.filter(r => r.status === s);
    }
    return { content: formatNetworkTable(filtered) };
  }, [flags]);

  return result;
}

async function getNetworkRequest(reqId) {
  const tab = await getActiveTab();
  return await execInTab(tab.id, (_reqId) => {
    const logs = window.__aic_network_logs || [];
    const req = logs.find(r => r.id === _reqId);
    if (!req) return { error: `request not found: ${_reqId}` };
    return { content: JSON.stringify(req, null, 2) };
  }, [reqId]);
}

async function clearNetworkLogs() {
  const tab = await getActiveTab();
  await execInTab(tab.id, () => {
    window.__aic_network_logs = [];
  }, []);
  return { content: "Network logs cleared" };
}

// format network logs as a table
function formatNetworkTable(logs) {
  if (!logs || logs.length === 0) return "(no network requests)";
  const lines = logs.map(r =>
    `${r.id} ${r.method} ${r.status} ${r.type} ${r.url}`
  );
  return lines.join("\n");
}

// ---- read ----

async function read(argv, timeout) {
  const url = argv[0];
  let readUrl = url;
  if (!readUrl) {
    const tab = await getActiveTab();
    readUrl = tab.url;
  }

  if (readUrl && (readUrl.startsWith("http://") || readUrl.startsWith("https://"))) {
    try {
      const resp = await fetch(readUrl);
      const html = await resp.text();
      // Very basic readability: strip tags and scripts
      const text = html
        .replace(/<script[^>]*>[\s\S]*?<\/script>/gi, "")
        .replace(/<style[^>]*>[\s\S]*?<\/style>/gi, "")
        .replace(/<[^>]+>/g, " ")
        .replace(/&nbsp;/g, " ")
        .replace(/&amp;/g, "&")
        .replace(/&lt;/g, "<")
        .replace(/&gt;/g, ">")
        .replace(/&quot;/g, '"')
        .replace(/\s{2,}/g, "\n")
        .trim();
      // Truncate to ~100KB
      const maxLen = 100_000;
      const truncated = text.length > maxLen ? text.slice(0, maxLen) + "\n... (truncated)" : text;
      return { content: truncated, attrs: truncated.length < text.length ? { truncated: "true" } : {} };
    } catch (err) {
      return { status: "error", error: `read failed: ${err.message}` };
    }
  }

  // Fallback: read current page text via script injection
  const tab = await getActiveTab();
  return await execInTab(tab.id, () => {
    const text = document.body.innerText || "";
    return { content: text.slice(0, 100_000) };
  }, []);
}

// ---- screenshot ----

async function screenshot(argv) {
  const flags = parseFlags(argv, {
    "--quality": "string", "--full": "flag",
  });
  const quality = Math.min(parseInt(flags["--quality"] || "60", 10), 60);

  if (flags["--full"]) {
    return await fullPageScreenshot(quality);
  }

  try {
    const tab = await getActiveTab();
    let dataUrl = await chrome.tabs.captureVisibleTab(tab.windowId, { format: "jpeg", quality });
    const b64 = dataUrl.replace(/^data:image\/jpeg;base64,/, "");

    const sizeBytes = Math.round(b64.length * 0.75);
    const sizeStr = sizeBytes > 1024 * 1024
      ? `${(sizeBytes / 1024 / 1024).toFixed(1)}MB`
      : `${Math.round(sizeBytes / 1024)}KB`;

    // Keep under 1MB: reduce quality if needed
    if (sizeBytes > 1_000_000) {
      const reducedQuality = Math.max(quality - 20, 10);
      dataUrl = await chrome.tabs.captureVisibleTab(tab.windowId, { format: "jpeg", quality: reducedQuality });
    }

    return {
      content: `${sizeStr} JPEG screenshot of ${tab.url}`,
      attrs: { image_data: dataUrl },
    };
  } catch (err) {
    return { status: "error", error: `screenshot failed: ${err.message}` };
  }
}

async function fullPageScreenshot(quality) {
  const tab = await getActiveTab();

  return new Promise(async (resolve) => {
    try {
      await chrome.debugger.attach({ tabId: tab.id }, "1.3");

      const metrics = await chrome.debugger.sendCommand(
        { tabId: tab.id }, "Page.getLayoutMetrics"
      );

      const { contentSize } = metrics.cssLayoutMetrics || {};
      const width = Math.ceil(contentSize?.width || 1280);
      const height = Math.ceil(contentSize?.height || 720);

      await chrome.debugger.sendCommand(
        { tabId: tab.id }, "Emulation.setDeviceMetricsOverride",
        { width, height, deviceScaleFactor: 1, mobile: false }
      );

      let result = await chrome.debugger.sendCommand(
        { tabId: tab.id }, "Page.captureScreenshot",
        { format: "jpeg", quality, clip: { x: 0, y: 0, width, height, scale: 1 } }
      );

      // Keep under 1MB
      const sizeBytes = Math.round(result.data.length * 0.75);
      if (sizeBytes > 1_000_000) {
        result = await chrome.debugger.sendCommand(
          { tabId: tab.id }, "Page.captureScreenshot",
          { format: "jpeg", quality: Math.max(quality - 20, 10), clip: { x: 0, y: 0, width, height, scale: 1 } }
        );
      }

      await chrome.debugger.sendCommand(
        { tabId: tab.id }, "Emulation.clearDeviceMetricsOverride"
      );

      await chrome.debugger.detach({ tabId: tab.id });

      const finalSize = Math.round(result.data.length * 0.75);
      const sizeStr = finalSize > 1024 * 1024
        ? `${(finalSize / 1024 / 1024).toFixed(1)}MB`
        : `${Math.round(finalSize / 1024)}KB`;

      resolve({
        content: `${sizeStr} JPEG full-page screenshot of ${tab.url} (${width}x${height})`,
        attrs: { width: String(width), height: String(height), image_data: `data:image/jpeg;base64,${result.data}` },
      });
    } catch (err) {
      try { await chrome.debugger.detach({ tabId: tab.id }); } catch (_) { }
      resolve({ status: "error", error: `full page screenshot failed: ${err.message}` });
    }
  });
}

// ---- snapshot (a11y tree) ----

async function snapshot(argv, timeout) {
  const flags = parseFlags(argv, {
    "-i": "flag", "-c": "flag",
    "-d": "string", "-s": "string",
  });
  const tab = await getActiveTab();

  const result = await execInTab(tab.id, (_flags) => {
    const interactive = !!_flags["-i"];
    const compact = !!_flags["-c"];
    const depth = _flags["-d"] ? parseInt(_flags["-d"], 10) : Infinity;
    const scope = _flags["-s"] || null;

    let uidCounter = 0;
    const lines = [];

    function isInteractive(el) {
      const tag = el.tagName.toLowerCase();
      const role = (el.getAttribute("role") || "").toLowerCase();
      const type = (el.getAttribute("type") || "").toLowerCase();

      if (["button", "a", "input", "select", "textarea", "option"].includes(tag)) return true;
      if (["button", "link", "checkbox", "radio", "textbox", "combobox", "menuitem", "tab", "switch", "option"].includes(role)) return true;
      if (el.hasAttribute("onclick") || el.getAttribute("tabindex") === "0") return true;
      if (tag === "input" && ["checkbox", "radio", "submit", "button", "reset"].includes(type)) return true;

      return false;
    }

    function getAriaName(el) {
      const label = el.getAttribute("aria-label") || el.getAttribute("title") || el.getAttribute("placeholder") || el.getAttribute("alt") || "";
      if (label) return label;
      // Use text content for links and buttons
      if (["a", "button"].includes(el.tagName.toLowerCase())) {
        const t = el.textContent.trim().slice(0, 50);
        if (t) return t;
      }
      return "";
    }

    function getRole(el) {
      const role = el.getAttribute("role");
      if (role) return role;
      const tag = el.tagName.toLowerCase();
      const type = (el.getAttribute("type") || "").toLowerCase();
      const roleMap = {
        a: "link", button: "button",
        input: type === "checkbox" ? "checkbox" : type === "radio" ? "radio" : type === "submit" || type === "button" ? "button" : "textbox",
        select: "combobox", textarea: "textbox",
        img: "img", h1: "heading", h2: "heading", h3: "heading", h4: "heading", h5: "heading", h6: "heading",
        nav: "navigation", main: "main", form: "form", table: "table",
        li: "listitem", ul: "list", ol: "list",
        section: "section", article: "article", aside: "complementary",
        header: "banner", footer: "contentinfo",
      };
      return roleMap[tag] || tag;
    }

    function formatAttrs(el) {
      const parts = [];
      const tag = el.tagName.toLowerCase();
      const type = (el.getAttribute("type") || "").toLowerCase();

      if (tag === "input" && type) parts.push(`type="${type}"`);
      if (el.hasAttribute("checked")) parts.push("checked");
      if (el.hasAttribute("disabled")) parts.push("disabled");
      if (el.hasAttribute("required")) parts.push("required");
      if (el.hasAttribute("readonly")) parts.push("readonly");
      if (el.hasAttribute("aria-expanded")) parts.push(`expanded="${el.getAttribute("aria-expanded")}"`);
      if (el.hasAttribute("aria-haspopup")) parts.push(`haspopup="${el.getAttribute("aria-haspopup")}"`);
      if (el.hasAttribute("aria-selected")) parts.push("selected");
      if (el.value !== undefined && el.value !== "" && (tag === "input" || tag === "textarea")) {
        parts.push(`value="${el.value.slice(0, 30)}"`);
      }
      if (el === document.activeElement) parts.push("focused");
      if (el.hasAttribute("href")) parts.push(`href="${el.getAttribute("href").slice(0, 50)}"`);

      return parts.length ? " " + parts.join(" ") : "";
    }

    function walk(el, d, indent) {
      if (d > depth || !el) return;
      // Skip invisible/hidden elements
      if (el.nodeType !== 1) return;

      const style = window.getComputedStyle(el);
      if (style.display === "none" || style.visibility === "hidden") return;

      const tag = el.tagName.toLowerCase();
      // Skip script, style, noscript, meta, link
      if (["script", "style", "noscript", "meta", "link", "br", "hr"].includes(tag)) return;
      // Skip tiny/spacer elements
      const rect = el.getBoundingClientRect();
      if (rect.width === 0 && rect.height === 0 && tag !== "a" && tag !== "button") return;

      if (interactive && !isInteractive(el)) {
        // Still walk children (interactive elements may be nested)
        for (const child of el.children) {
          walk(child, d, indent);
        }
        return;
      }

      if (compact && !isInteractive(el) && el.children.length > 0) {
        // Skip non-interactive structural wrappers
        for (const child of el.children) {
          walk(child, d, indent);
        }
        return;
      }

      const uid = uidCounter++;
      el.setAttribute("data-aic-ref", String(uid));

      const role = getRole(el);
      const name = getAriaName(el);
      const attrs = formatAttrs(el);

      let line = `uid=${uid} ${role}`;
      if (name) line += ` "${name}"`;
      if (attrs) line += attrs;

      lines.push("  ".repeat(indent) + line);

      // Walk children
      for (const child of el.children) {
        walk(child, d + 1, indent + 1);
      }
    }

    const root = scope ? document.querySelector(scope) : document.body;
    if (!root) return { error: scope ? `scope not found: ${scope}` : "no body" };
    walk(root, 0, 0);

    return { content: lines.join("\n") };
  }, [flags]);

  if (result?.error) return { status: "error", error: result.error };
  return result;
}

// ---- tab management ----

async function tab(argv) {
  const subAction = argv[0];
  if (!subAction) return { status: "error", error: "tab requires a sub-action: new, list, close, or <N>" };

  switch (subAction) {
    case "new": {
      const incognito = await getIncognito();
      await chrome.tabs.create({ active: true });
      return { content: "New tab created" };
    }
    case "list": {
      const tabs = await chrome.tabs.query({ currentWindow: true });
      const lines = tabs.map((t, i) => `${i}: ${t.title || t.url} [${t.active ? "active" : "inactive"}]`);
      return { content: lines.join("\n") };
    }
    case "close": {
      const tabs = await chrome.tabs.query({ currentWindow: true });
      if (tabs.length <= 1) return { content: "Cannot close last tab" };
      const [active] = await chrome.tabs.query({ active: true, currentWindow: true });
      if (active) await chrome.tabs.remove(active.id);
      return { content: `Tab closed` };
    }
    default: {
      const n = parseInt(subAction, 10);
      if (!isNaN(n)) {
        const tabs = await chrome.tabs.query({ currentWindow: true });
        if (n >= 0 && n < tabs.length) {
          await chrome.tabs.update(tabs[n].id, { active: true });
          return { content: `Switched to tab ${n}: ${tabs[n].title || tabs[n].url}` };
        }
        return { status: "error", error: `tab index ${n} out of range (0-${tabs.length - 1})` };
      }
      return { status: "error", error: `unknown tab action: ${subAction}` };
    }
  }
}

async function getIncognito() {
  try {
    const s = await chrome.storage.local.get("settings");
    return s.settings?.incognito || false;
  } catch { return false; }
}

// ---- wait ----

async function wait(argv, timeout) {
  if (argv.length < 1) return { status: "error", error: "wait requires a condition" };

  const flags = parseFlags(argv, {
    "--url": "string", "--load": "string",
    "--fn": "string", "--text": "string",
  });

  const tab = await getActiveTab();

  // --load condition
  if (flags["--load"]) {
    return await waitForLoadCondition(tab.id, flags["--load"], timeout);
  }

  // --url condition
  if (flags["--url"]) {
    return await waitForUrl(tab.id, flags["--url"], timeout);
  }

  // --fn condition
  if (flags["--fn"]) {
    return await waitForFn(tab.id, flags["--fn"], timeout);
  }

  // --text condition
  if (flags["--text"]) {
    return await waitForText(tab.id, flags["--text"], timeout);
  }

  // First arg: selector or ms
  const first = argv[0];
  if (first.startsWith("@") || !/^\d+$/.test(first)) {
    // CSS selector or @ref
    const sel = await resolveRefSelector(tab.id, first);
    return await waitForSelector(tab.id, sel, timeout);
  }

  // Milliseconds
  const ms = parseInt(first, 10);
  return await sleepMs(ms);
}

async function waitForSelector(tabId, sel, timeout) {
  const result = await execInTab(tabId, (_sel, _timeout) => {
    return new Promise(resolve => {
      const start = Date.now();
      function check() {
        const el = document.querySelector(_sel);
        if (el) {
          resolve({ content: `Element found: ${_sel}` });
          return;
        }
        if (Date.now() - start > _timeout) {
          resolve({ error: `timeout waiting for selector: ${_sel}` });
          return;
        }
        setTimeout(check, 100);
      }
      check();
    });
  }, [sel, timeout]);
  return result;
}

async function waitForUrl(tabId, glob, timeout) {
  return new Promise(resolve => {
    const start = Date.now();
    const pattern = glob.replace(/\*/g, ".*").replace(/\?/g, ".");
    const re = new RegExp(pattern);
    const timer = setInterval(async () => {
      try {
        const tab = await chrome.tabs.get(tabId);
        if (re.test(tab.url)) {
          clearInterval(timer);
          resolve({ content: `URL matched: ${tab.url}` });
          return;
        }
        if (Date.now() - start > timeout) {
          clearInterval(timer);
          resolve({ error: `timeout waiting for URL: ${glob}` });
        }
      } catch {
        clearInterval(timer);
        resolve({ error: "tab closed while waiting" });
      }
    }, 200);
  });
}

async function waitForLoadCondition(tabId, condition, timeout) {
  return new Promise(resolve => {
    const maxWait = timeout || 30_000;
    const timer = setTimeout(() => resolve({ content: `Waited ${maxWait}ms for ${condition}` }), maxWait);

    if (condition === "domcontentloaded") {
      const listener = (updatedTabId, info) => {
        if (updatedTabId === tabId && info.status === "complete") {
          clearTimeout(timer);
          chrome.tabs.onUpdated.removeListener(listener);
          resolve({ content: "DOM content loaded" });
        }
      };
      chrome.tabs.onUpdated.addListener(listener);
    } else if (condition === "networkidle") {
      // Wait for page load complete + additional quiet period
      let lastActivity = Date.now();
      const listener = (updatedTabId, info) => {
        if (updatedTabId === tabId) lastActivity = Date.now();
      };
      chrome.tabs.onUpdated.addListener(listener);
      const checker = setInterval(() => {
        if (Date.now() - lastActivity > 2000) {
          clearInterval(checker);
          clearTimeout(timer);
          chrome.tabs.onUpdated.removeListener(listener);
          resolve({ content: "Network idle" });
        }
      }, 500);
    } else {
      clearTimeout(timer);
      resolve({ error: `unknown load condition: ${condition}` });
    }
  });
}

async function waitForFn(tabId, js, timeout) {
  return await execInTab(tabId, (_js, _timeout) => {
    return new Promise(resolve => {
      const start = Date.now();
      function check() {
        try {
          const result = eval(_js);
          if (result) {
            resolve({ content: `Condition met: ${_js}` });
            return;
          }
        } catch (_) { }
        if (Date.now() - start > _timeout) {
          resolve({ error: `timeout waiting for condition: ${_js}` });
          return;
        }
        setTimeout(check, 200);
      }
      check();
    });
  }, [js, timeout]);
}

async function waitForText(tabId, text, timeout) {
  return await execInTab(tabId, (_text, _timeout) => {
    return new Promise(resolve => {
      const start = Date.now();
      function check() {
        if (document.body.innerText.includes(_text)) {
          resolve({ content: `Text found: "${_text}"` });
          return;
        }
        if (Date.now() - start > _timeout) {
          resolve({ error: `timeout waiting for text: "${_text}"` });
          return;
        }
        setTimeout(check, 200);
      }
      check();
    });
  }, [text, timeout]);
}

// ---- scroll ----

async function scroll(argv, timeout) {
  const dir = argv[0];
  if (!dir || !["up", "down", "left", "right"].includes(dir)) {
    return { status: "error", error: "scroll requires direction: up, down, left, right" };
  }
  const px = argv[1] ? parseInt(argv[1], 10) : 600;

  const tab = await getActiveTab();
  return await execInTab(tab.id, (_dir, _px) => {
    const scrollOpts = { top: 0, left: 0, behavior: "smooth" };
    switch (_dir) {
      case "down": scrollOpts.top = window.scrollY + _px; break;
      case "up": scrollOpts.top = window.scrollY - _px; break;
      case "right": scrollOpts.left = window.scrollX + _px; break;
      case "left": scrollOpts.left = window.scrollX - _px; break;
    }
    window.scrollBy(scrollOpts);
    return { content: `Scrolled ${_dir} ${_px}px` };
  }, [dir, px]);
}

// ---- hover ----

async function hover(argv, timeout) {
  if (argv.length < 1) return { status: "error", error: "hover requires a selector" };
  const tab = await getActiveTab();
  const sel = await resolveRefSelector(tab.id, argv[0]);
  return await execInTab(tab.id, (_sel) => {
    const el = document.querySelector(_sel);
    if (!el) return { error: `element not found: ${_sel}` };
    el.scrollIntoView({ block: "center" });
    el.dispatchEvent(new MouseEvent("mouseenter", { bubbles: true }));
    el.dispatchEvent(new MouseEvent("mouseover", { bubbles: true }));
    return { content: `Hovered ${_sel}` };
  }, [sel]);
}

// ---- fill ----

async function fill(argv, timeout) {
  if (argv.length < 2) return { status: "error", error: "fill requires selector and text" };
  const tab = await getActiveTab();
  const sel = await resolveRefSelector(tab.id, argv[0]);
  const text = argv[1];

  const result = await execInTab(tab.id, (_sel, _text) => {
    const el = document.querySelector(_sel);
    if (!el) return { error: `element not found: ${_sel}` };

    el.scrollIntoView({ block: "center" });
    el.focus();

    // Clear existing value
    if (el.tagName === "INPUT" || el.tagName === "TEXTAREA") {
      el.value = "";
      el.dispatchEvent(new Event("input", { bubbles: true }));
    }

    // Set new value
    if (el.tagName === "INPUT" || el.tagName === "TEXTAREA") {
      el.value = _text;
      el.dispatchEvent(new Event("input", { bubbles: true }));
      el.dispatchEvent(new Event("change", { bubbles: true }));
    } else {
      el.textContent = _text;
    }

    return { content: `Filled ${_sel} with "${_text}"` };
  }, [sel, text]);

  if (result.error) return { status: "error", error: result.error };
  return result;
}

// ---- press ----

async function press(argv, timeout) {
  if (argv.length < 1) return { status: "error", error: "press requires a key" };
  const key = argv[0];
  const tab = await getActiveTab();

  return await execInTab(tab.id, (_key) => {
    const active = document.activeElement || document.body;
    const modifiers = {};
    const parts = _key.split("+");
    let k = parts.pop();

    // Normalize key name
    const keyMap = {
      enter: "Enter", tab: "Tab", escape: "Escape",
      arrowdown: "ArrowDown", arrowup: "ArrowUp", arrowleft: "ArrowLeft", arrowright: "ArrowRight",
      backspace: "Backspace", delete: "Delete", space: " ",
      pageup: "PageUp", pagedown: "PageDown", home: "Home", end: "End",
    };

    k = keyMap[k.toLowerCase()] || k;

    for (const p of parts) {
      const m = p.toLowerCase();
      if (m === "control" || m === "ctrl") modifiers.ctrlKey = true;
      if (m === "alt") modifiers.altKey = true;
      if (m === "shift") modifiers.shiftKey = true;
      if (m === "meta" || m === "cmd") modifiers.metaKey = true;
    }

    active.dispatchEvent(new KeyboardEvent("keydown", { key: k, ...modifiers, bubbles: true }));
    active.dispatchEvent(new KeyboardEvent("keypress", { key: k, ...modifiers, bubbles: true }));
    active.dispatchEvent(new KeyboardEvent("keyup", { key: k, ...modifiers, bubbles: true }));

    return { content: `Pressed ${_key}` };
  }, [key]);
}

// ---- select ----

async function selectDropdown(argv, timeout) {
  if (argv.length < 2) return { status: "error", error: "select requires selector and value" };
  const tab = await getActiveTab();
  const sel = await resolveRefSelector(tab.id, argv[0]);
  const value = argv[1];

  const result = await execInTab(tab.id, (_sel, _value) => {
    const el = document.querySelector(_sel);
    if (!el) return { error: `element not found: ${_sel}` };
    if (el.tagName !== "SELECT") return { error: `element is not a select: ${_sel}` };

    // Try to match by value first, then by text
    for (const opt of el.options) {
      if (opt.value === _value || opt.textContent.trim() === _value) {
        opt.selected = true;
        el.dispatchEvent(new Event("change", { bubbles: true }));
        return { content: `Selected "${_value}" in ${_sel}` };
      }
    }
    return { error: `option not found: "${_value}" in ${_sel}` };
  }, [sel, value]);

  if (result.error) return { status: "error", error: result.error };
  return result;
}

// ---- navigation ----

async function navigation(action) {
  const tab = await getActiveTab();
  switch (action) {
    case "back":
      if (tab.id) await chrome.tabs.goBack(tab.id);
      return { content: "Navigated back" };
    case "forward":
      if (tab.id) await chrome.tabs.goForward(tab.id);
      return { content: "Navigated forward" };
    case "reload":
      if (tab.id) await chrome.tabs.reload(tab.id);
      return { content: "Page reloaded" };
    default:
      return { status: "error", error: `unknown navigation: ${action}` };
  }
}

// ---- sleep ----

async function sleep(argv) {
  const duration = argv[0] || "1s";
  return await sleepMs(parseDuration(duration));
}

async function sleepMs(ms) {
  await new Promise(r => setTimeout(r, ms));
  return { content: `Slept for ${ms}ms` };
}

function parseDuration(s) {
  s = s.toLowerCase().trim();
  if (s.endsWith("ms")) return parseInt(s, 10) || 0;
  if (s.endsWith("s")) return (parseFloat(s) || 0) * 1000;
  if (s.endsWith("m")) return (parseFloat(s) || 0) * 60_000;
  if (s.endsWith("h")) return (parseFloat(s) || 0) * 3_600_000;
  return parseInt(s, 10) || 0;
}

// ---- cookies ----

async function cookies(argv) {
  const subAction = argv[0];
  if (!subAction) return { status: "error", error: "cookies requires sub-action: get, set, clear" };

  const flags = parseFlags(argv, {
    "--name": "string", "--domain": "string",
    "--value": "string", "--path": "string",
    "--httpOnly": "flag", "--secure": "flag", "--sameSite": "string",
    "--expires": "string",
  });

  const tab = await getActiveTab();
  const url = tab.url;

  switch (subAction) {
    case "get": {
      const details = { url };
      if (flags["--name"]) details.name = flags["--name"];
      if (flags["--domain"]) details.domain = flags["--domain"];

      if (flags["--name"]) {
        const cookie = await chrome.cookies.get(details);
        return { content: cookie ? JSON.stringify(cookie, null, 2) : "(no cookie)" };
      }
      const cookies = await chrome.cookies.getAll(details);
      const lines = cookies.map(c => `${c.name}=${c.value} [domain=${c.domain}, path=${c.path}]`);
      return { content: lines.length ? lines.join("\n") : "(no cookies)" };
    }

    case "set": {
      if (!flags["--name"] || flags["--value"] === undefined) {
        return { status: "error", error: "cookies set requires --name and --value" };
      }
      const details = {
        url,
        name: flags["--name"],
        value: flags["--value"],
        domain: flags["--domain"],
        path: flags["--path"] || "/",
        httpOnly: !!flags["--httpOnly"],
        secure: !!flags["--secure"],
        sameSite: flags["--sameSite"] || "lax",
      };
      if (flags["--expires"]) {
        details.expirationDate = new Date(flags["--expires"]).getTime() / 1000;
      }
      const cookie = await chrome.cookies.set(details);
      return { content: `Cookie set: ${cookie.name}=${cookie.value}` };
    }

    case "clear": {
      const details = { url };
      if (flags["--name"]) details.name = flags["--name"];

      if (flags["--name"]) {
        await chrome.cookies.remove(details);
        return { content: `Cookie removed: ${flags["--name"]}` };
      }
      const all = await chrome.cookies.getAll(details);
      for (const c of all) {
        await chrome.cookies.remove({ url, name: c.name });
      }
      return { content: `Removed ${all.length} cookies` };
    }

    default:
      return { status: "error", error: `unknown cookies sub-action: ${subAction}` };
  }
}

// ---- storage ----

async function storage(argv, timeout) {
  if (argv.length < 2) return { status: "error", error: "storage requires type and action" };
  const storageType = argv[0].toLowerCase();
  if (storageType !== "local" && storageType !== "session") {
    return { status: "error", error: "storage type must be local or session" };
  }
  const subAction = argv[1];
  const flags = parseFlags(argv, { "--key": "string", "--value": "string" });

  const tab = await getActiveTab();

  return await execInTab(tab.id, (_type, _action, _key, _value) => {
    const store = _type === "local" ? localStorage : sessionStorage;

    switch (_action) {
      case "get": {
        if (!_key) {
          // List all keys
          const keys = [];
          for (let i = 0; i < store.length; i++) {
            keys.push(store.key(i));
          }
          return { content: keys.length ? keys.join("\n") : "(empty)" };
        }
        const val = store.getItem(_key);
        return { content: val !== null ? val : "(null)" };
      }
      case "set": {
        if (!_key || _value === undefined) return { error: "storage set requires --key and --value" };
        store.setItem(_key, _value);
        return { content: `${_type}Storage: ${_key} = ${_value}` };
      }
      case "del": {
        if (!_key) return { error: "storage del requires --key" };
        store.removeItem(_key);
        return { content: `${_type}Storage: removed ${_key}` };
      }
      default:
        return { error: `unknown storage action: ${_action}` };
    }
  }, [storageType, subAction, flags["--key"], flags["--value"]]);
}

// ---- pipeline (TODO) ----

// async function pipeline(argv, timeout) { ... }
// function splitPipeline(args) { ... }
