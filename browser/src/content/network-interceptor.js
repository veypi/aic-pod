/**
 * network-interceptor.js — Content script injected into all pages
 *
 * Hooks fetch() and XMLHttpRequest to collect network logs.
 * Logs are stored in window.__aic_network_logs for the Service Worker to read.
 */

(function () {
  if (window.__aic_network_init) return;
  window.__aic_network_init = true;

  const MAX_LOGS = 500; // §5.5 环形缓冲：每页会话最多保留最近 500 条请求
  window.__aic_network_logs = [];

  let reqCounter = 0;

  function addLog(entry) {
    window.__aic_network_logs.push(entry);
    if (window.__aic_network_logs.length > MAX_LOGS) {
      window.__aic_network_logs.shift();
    }
  }

  // ---- Intercept fetch() ----
  const origFetch = window.fetch;
  window.fetch = async function (input, init) {
    const url = typeof input === "string" ? input : (input.url || input.href);
    const method = (init?.method || "GET").toUpperCase();
    const id = `req-${++reqCounter}`;
    const start = Date.now();

    addLog({ id, url, method, type: "fetch", status: 0, start, end: 0 });

    try {
      const resp = await origFetch.apply(this, arguments);
      addLog({ id, url, method, type: "fetch", status: resp.status, start, end: Date.now() });
      return resp;
    } catch (err) {
      addLog({ id, url, method, type: "fetch", status: 0, start, end: Date.now(), error: err.message });
      throw err;
    }
  };

  // ---- Intercept XMLHttpRequest ----
  const OrigXHR = window.XMLHttpRequest;
  window.XMLHttpRequest = function () {
    const xhr = new OrigXHR();
    const id = `req-${++reqCounter}`;
    let method = "GET";
    let url = "";
    const start = Date.now();

    const origOpen = xhr.open;
    xhr.open = function (m, u, ...rest) {
      method = m.toUpperCase();
      url = typeof u === "string" ? u : (u.url || u.href || "");
      return origOpen.apply(this, [m, u, ...rest]);
    };

    xhr.addEventListener("loadend", () => {
      addLog({ id, url, method, type: "xhr", status: xhr.status, start, end: Date.now() });
    });

    return xhr;
  };
  window.XMLHttpRequest.prototype = OrigXHR.prototype;
})();
