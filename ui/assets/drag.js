// drag.js — 桌面 frameless 窗口拖动（仅 wails 桥存在时生效）
// 与 wails3 runtime.js 同协议：mousedown 在拖动区（.shell-drag）→ 发送
// "wails:drag" 消息 → Go 侧 startDrag()；双击 → "wails:drag:doubleclick"
// （mac 双击标题栏最大化语义）。
// 三平台桥：win = chrome.webview.postMessage；mac/linux-gtk = 
// webkit.messageHandlers.external.postMessage；兜底 window.wails.invoke。
// 非桌面（普通浏览器）桥不存在，函数为 no-op。

export default function dragInit(enabled) {
  if (!enabled) return
  const bridge =
    (window.chrome && window.chrome.webview && window.chrome.webview.postMessage)
      ? window.chrome.webview
      : (window.webkit && window.webkit.messageHandlers && window.webkit.messageHandlers.external)
        ? window.webkit.messageHandlers.external
        : (window.wails && window.wails.invoke)
          ? { postMessage: (m) => window.wails.invoke(m) }
          : null
  if (!bridge) return

  const inDrag = (t) =>
    t && t.closest && t.closest(".shell-drag") && !t.closest("button,a,input,select,textarea")

  document.addEventListener("mousedown", (e) => {
    if (inDrag(e.target)) bridge.postMessage("wails:drag")
  })
  document.addEventListener("dblclick", (e) => {
    if (inDrag(e.target)) bridge.postMessage("wails:drag:doubleclick")
  })
}
