/**
 * options.js — Settings page logic for AIC Browser Extension
 */

import { loadSettings, saveSettings } from "../src/sdk/storage.js";

const form = document.getElementById("settings-form");
const statusEl = document.getElementById("status");
const connIndicator = document.getElementById("conn-indicator");
const connText = document.getElementById("conn-text");
const connectBtn = document.getElementById("connect-btn");
const disconnectBtn = document.getElementById("disconnect-btn");

// ---- Load settings into form ----

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

// ---- Save settings ----

form.addEventListener("submit", async (e) => {
  e.preventDefault();
  const settings = {
    key: document.getElementById("key").value.trim(),
    url: document.getElementById("url").value.trim() || "wss://ivec.ai/aic/api/nc",
    deviceName: document.getElementById("deviceName").value.trim(),
    autoConnect: document.getElementById("autoConnect").checked,
    background: document.getElementById("background").checked,
    incognito: document.getElementById("incognito").checked,
    viewport: {
      width: parseInt(document.getElementById("viewportWidth").value, 10) || 1280,
      height: parseInt(document.getElementById("viewportHeight").value, 10) || 720,
    },
    timeout: parseInt(document.getElementById("timeout").value, 10) || 30,
  };

  await saveSettings(settings);
  showStatus("设置已保存 ✓", "success");
});

// ---- Connection controls ----

connectBtn.addEventListener("click", async () => {
  chrome.runtime.sendMessage({ type: "connect" }, (resp) => {
    if (resp?.success) {
      updateConnectionStatus(true);
      showStatus("已连接 ✓", "success");
    } else {
      showStatus("连接失败: " + (resp?.error || "unknown"), "error");
    }
  });
});

disconnectBtn.addEventListener("click", async () => {
  chrome.runtime.sendMessage({ type: "disconnect" }, (resp) => {
    if (resp?.success) {
      updateConnectionStatus(false);
      showStatus("已断开", "success");
    }
  });
});

// ---- Status polling ----

async function checkConnection() {
  try {
    chrome.runtime.sendMessage({ type: "status" }, (resp) => {
      if (chrome.runtime.lastError) {
        updateConnectionStatus(false);
        return;
      }
      updateConnectionStatus(resp?.connected || false);
    });
  } catch {
    updateConnectionStatus(false);
  }
}

function updateConnectionStatus(connected) {
  if (connected) {
    connIndicator.className = "indicator connected";
    connText.textContent = "已连接";
    connectBtn.style.display = "none";
    disconnectBtn.style.display = "";
  } else {
    connIndicator.className = "indicator disconnected";
    connText.textContent = "未连接";
    connectBtn.style.display = "";
    disconnectBtn.style.display = "none";
  }
}

function showStatus(msg, type) {
  statusEl.textContent = msg;
  statusEl.style.color = type === "error" ? "#e53e3e" : type === "success" ? "#22c55e" : "#666";
  setTimeout(() => { statusEl.textContent = ""; }, 3000);
}

// ---- Init ----

populateForm();
checkConnection();
setInterval(checkConnection, 5000);
