/**
 * popup.js — Browser action popup for quick status and toggle settings
 */

import { loadSettings, saveSettings } from "../src/sdk/storage.js";

// ---- elements ----
const indicator = document.getElementById("indicator");
const statusText = document.getElementById("status-text");
const autoConnectEl = document.getElementById("auto-connect");
const backgroundEl = document.getElementById("background");
const incognitoEl = document.getElementById("incognito");
const connectBtn = document.getElementById("connect-btn");
const disconnectBtn = document.getElementById("disconnect-btn");
const optionsBtn = document.getElementById("options-btn");

// ---- init ----
let settings = await loadSettings();

autoConnectEl.checked = settings.autoConnect !== false;
backgroundEl.checked = settings.background !== false;
incognitoEl.checked = !!settings.incognito;

updateConnectionUI(false);
checkConnection();

// ---- toggle save ----
for (const [el, key] of [
  [autoConnectEl, "autoConnect"],
  [backgroundEl, "background"],
  [incognitoEl, "incognito"],
]) {
  el.addEventListener("change", async () => {
    settings[key] = el.checked;
    await saveSettings(settings);
  });
}

// ---- buttons ----
connectBtn.addEventListener("click", () => {
  chrome.runtime.sendMessage({ type: "connect" }, (resp) => {
    if (chrome.runtime.lastError) return;
    if (resp?.success) updateConnectionUI(true);
  });
});

disconnectBtn.addEventListener("click", () => {
  chrome.runtime.sendMessage({ type: "disconnect" }, (resp) => {
    if (chrome.runtime.lastError) return;
    if (resp?.success) updateConnectionUI(false);
  });
});

optionsBtn.addEventListener("click", () => {
  chrome.runtime.openOptionsPage();
});

// ---- status ----
function checkConnection() {
  chrome.runtime.sendMessage({ type: "status" }, (resp) => {
    if (chrome.runtime.lastError) { updateConnectionUI(false); return; }
    updateConnectionUI(!!resp?.connected);
  });
}

function updateConnectionUI(connected) {
  if (connected) {
    indicator.className = "dot connected";
    statusText.textContent = "已连接";
    connectBtn.style.display = "none";
    disconnectBtn.style.display = "";
  } else {
    indicator.className = "dot disconnected";
    statusText.textContent = "未连接";
    connectBtn.style.display = "";
    disconnectBtn.style.display = "none";
  }
}
