// AIC Desktop 管理页：配置 + host 会话生命周期 + 日志
import { Events } from "@wailsio/runtime";
import { App } from "../bindings/aic-desktop/index";

const $ = (id) => document.getElementById(id);
const inHost = $("in-host"), inKey = $("in-key");

async function loadConfig() {
  try {
    const cfg = await App.GetConfig();
    inHost.value = cfg.host || "https://ivec.ai";
    inKey.value = cfg.credential || "";
  } catch (e) {
    console.error("load config:", e);
  }
}

$("btn-save").addEventListener("click", async () => {
  try {
    await App.SaveConfig({ host: inHost.value.trim(), credential: inKey.value.trim() });
    const h = $("save-hint");
    h.textContent = "已保存 ✓";
    setTimeout(() => (h.textContent = ""), 2000);
  } catch (e) {
    alert("保存失败: " + e);
  }
});

async function refreshStatus() {
  try {
    const st = await App.HostStatusQuery();
    const el = $("host-state");
    el.className = "state " + (st.running ? "running" : "idle");
    el.textContent = st.running ? "运行中" : "未运行";
  } catch (e) {
    $("host-state").className = "state error";
    $("host-state").textContent = "状态查询失败";
  }
}

$("btn-start").addEventListener("click", async () => {
  const btn = $("btn-start");
  btn.disabled = true;
  try {
    await App.StartHost(inHost.value.trim(), inKey.value.trim());
    await refreshStatus();
  } catch (e) {
    $("host-state").className = "state error";
    $("host-state").textContent = String(e);
  } finally {
    btn.disabled = false;
  }
});

$("btn-stop").addEventListener("click", async () => {
  await App.StopHost();
  await refreshStatus();
});

$("btn-open").addEventListener("click", async () => {
  try {
    await App.OpenPlatform(inHost.value.trim());
  } catch (e) {
    alert("打开平台失败: " + e);
  }
});

// 日志：事件实时追加 + 启动时拉取缓冲
const pre = $("host-log");
function appendLog(text) {
  if (!text) return;
  const atBottom = pre.scrollHeight - pre.scrollTop - pre.clientHeight < 40;
  pre.textContent += text + "\n";
  if (pre.textContent.length > 200000) {
    pre.textContent = pre.textContent.slice(-100000);
  }
  if (atBottom) pre.scrollTop = pre.scrollHeight;
}
Events.On("host-log", (line) => appendLog(line));

async function loadLog() {
  try {
    const text = await App.HostLog();
    pre.textContent = text || "";
    pre.scrollTop = pre.scrollHeight;
  } catch (_) {}
}

loadConfig();
loadLog();
refreshStatus();
setInterval(refreshStatus, 2000);
