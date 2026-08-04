/**
 * local-bridge.js — 平台 /settings/local 页面与扩展 background 的桥
 *
 * 注入 data-aic-ext="1" 标记（页面 local_handler.js 据此走插件通道），
 * 双向转发 window postMessage ↔ chrome.runtime message。
 * 安全：background 校验 localCode（页面须持有经 URL 下发的随机码），
 * 任意网页拿不到 code，调用一律被拒。
 *
 * isolated world（需要 chrome.runtime），与 MAIN world 的 network-interceptor 互补。
 */

function mark() {
  if (document.documentElement) {
    document.documentElement.dataset.aicExt = "1";
  } else {
    // document_start 早期 html 元素可能未创建
    addEventListener("DOMContentLoaded", mark, { once: true });
  }
}
mark();

window.addEventListener("message", (ev) => {
  if (ev.source !== window) return;
  const d = ev.data;
  if (!d || d.type !== "__aic_local" || !d.requestId) return;
  chrome.runtime.sendMessage(
    { type: "__aic_local", requestId: d.requestId, method: d.method, args: d.args, code: d.code },
    (resp) => {
      const out = { type: "__aic_local_resp", requestId: d.requestId };
      if (chrome.runtime.lastError) {
        out.error = chrome.runtime.lastError.message;
      } else if (resp?.error) {
        out.error = resp.error;
      } else {
        out.data = resp?.data;
      }
      window.postMessage(out, "*");
    }
  );
});
