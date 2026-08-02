/**
 * options.js — 设置页（连接 + 浏览器，合并为单一配置页）。
 * 侧栏与连接状态轮询由 common.js 统一处理。
 */

import { loadSettings, saveSettings } from "../src/sdk/storage.js";
import { showStatus, updateConnectionStatus } from "./common.js";

const connForm = document.getElementById("settings-form");
const browserForm = document.getElementById("browser-form");
const statusEl = document.getElementById("status");
const browserStatusEl = document.getElementById("browser-status");
const connText = document.getElementById("conn-text");
const connectBtn = document.getElementById("connect-btn");
const disconnectBtn = document.getElementById("disconnect-btn");

// ---- Load settings into forms ----

async function populateForm() {
  const s = await loadSettings();
  document.getElementById("key").value = s.key || "";
  document.getElementById("url").value = s.url || "wss://ivec.ai/aic/api/nc";
  document.getElementById("deviceName").value = s.deviceName || "";
  document.getElementById("autoConnect").checked = s.autoConnect !== false;
  document.getElementById("background").checked = s.background !== false;
  document.getElementById("incognito").checked = s.incognito || false;
  document.getElementById("viewportWidth").value = s.viewport?.width || 1280;
  document.getElementById("viewportHeight").value = s.viewport?.height || 720;
  document.getElementById("timeout").value = s.timeout || 30;
}

// ---- Save settings（两个表单分区保存） ----

connForm.addEventListener("submit", async (e) => {
  e.preventDefault();
  const settings = await loadSettings();
  settings.key = document.getElementById("key").value.trim();
  settings.url = document.getElementById("url").value.trim() || "wss://ivec.ai/aic/api/nc";
  settings.deviceName = document.getElementById("deviceName").value.trim();
  await saveSettings(settings);
  showStatus(statusEl, "设置已保存 ✓", "success");
});

browserForm.addEventListener("submit", async (e) => {
  e.preventDefault();
  const settings = await loadSettings();
  settings.autoConnect = document.getElementById("autoConnect").checked;
  settings.background = document.getElementById("background").checked;
  settings.incognito = document.getElementById("incognito").checked;
  settings.viewport = {
    width: parseInt(document.getElementById("viewportWidth").value, 10) || 1280,
    height: parseInt(document.getElementById("viewportHeight").value, 10) || 720,
  };
  settings.timeout = parseInt(document.getElementById("timeout").value, 10) || 30;
  await saveSettings(settings);
  showStatus(browserStatusEl, "设置已保存 ✓", "success");
});

// ---- Connection controls ----

connectBtn.addEventListener("click", async () => {
  connText.textContent = "连接中…";
  chrome.runtime.sendMessage({ type: "connect" }, (resp) => {
    if (resp?.success) {
      refreshConnButtons(true);
      showStatus(statusEl, "已连接 ✓", "success");
    } else {
      refreshConnButtons(false);
      showStatus(statusEl, "连接失败: " + (resp?.error || "unknown"), "error");
    }
  });
});

disconnectBtn.addEventListener("click", async () => {
  chrome.runtime.sendMessage({ type: "disconnect" }, (resp) => {
    if (resp?.success) {
      refreshConnButtons(false);
      showStatus(statusEl, "已断开", "success");
    }
  });
});

// 连接按钮显隐（侧栏指示灯由 common.js 轮询统一维护，这里同步一次按钮态）
function refreshConnButtons(connected) {
  updateConnectionStatus(connected);
  connectBtn.style.display = connected ? "none" : "";
  disconnectBtn.style.display = connected ? "" : "none";
}

chrome.runtime.sendMessage({ type: "status" }, (resp) => {
  if (!chrome.runtime.lastError) refreshConnButtons(resp?.connected === true);
});

// ---- Init ----

populateForm();
