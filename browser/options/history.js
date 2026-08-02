/**
 * history.js — 执行历史页：展示 background client 的环形请求记录。
 */

import { escapeHtml, truncate } from "./common.js";

const historyBody = document.getElementById("history-body");
const historyCount = document.getElementById("history-count");

document.getElementById("history-refresh").addEventListener("click", loadHistory);
document.getElementById("history-clear").addEventListener("click", () => {
  if (!confirm("清空全部执行历史？")) return;
  chrome.runtime.sendMessage({ type: "history:clear" }, () => loadHistory());
});

const STATE_CLASS = {
  completed: "ok",
  error: "bad",
  rejected: "bad",
  waiting: "wait",
  pending: "muted",
};

function loadHistory() {
  chrome.runtime.sendMessage({ type: "history" }, (resp) => {
    const rows = resp?.history || [];
    historyCount.textContent = `${rows.length} 条`;
    if (rows.length === 0) {
      historyBody.innerHTML = '<tr><td colspan="6" class="empty">暂无记录（连接后收到的请求会显示在这里）</td></tr>';
      return;
    }
    historyBody.innerHTML = rows
      .slice()
      .reverse()
      .map((h) => {
        const time = new Date(h.time).toLocaleTimeString("zh-CN", { hour12: false });
        const state = h.state || "pending";
        const err = h.error
          ? `<span class="err-text" title="${escapeHtml(h.error)}">${escapeHtml(truncate(h.error, 40))}</span>`
          : "";
        return `<tr>
          <td class="muted">${time}</td>
          <td>${escapeHtml(h.tool)}</td>
          <td>${escapeHtml(h.action)}</td>
          <td class="muted" title="${escapeHtml(h.sid)}">${escapeHtml(truncate(h.sid, 12))}</td>
          <td><span class="state ${STATE_CLASS[state] || "muted"}">${escapeHtml(state)}</span></td>
          <td>${err}</td>
        </tr>`;
      })
      .join("");
  });
}

loadHistory();
