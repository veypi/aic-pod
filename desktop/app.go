package main

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/veypi/aic-pod/sdk/host"
	"github.com/veypi/vigo/logv"
	"github.com/wailsapp/wails/v3/pkg/application"
)

// App 是桌面壳：host 会话生命周期（委托 sdk/host.Runner）+ 平台窗口。
// 配置读写与本地管理 API 全部在 sdk/host（cli/desktop 共享同一份 config.yaml
// 与同一套 local_code 通道）；desktop 只加 Wails 壳 + 首页窗口连接。
type App struct {
	runner   *host.Runner   // host 生命周期（"desktop" 类型）
	local    *host.LocalAPI  // 本地 HTTP 服务（local_code 通道）
	cfg      host.Config    // 启动时解析的有效配置（flag/env/文件，与 cli 同一套解析）
	platform application.Window // 平台窗口（托盘打开/聚焦用，单窗口）
}

// NewApp 创建桌面壳。cfg 为 flags 解析后的有效配置。
func NewApp(cfg host.Config) *App {
	return &App{runner: host.NewRunner("desktop", version), cfg: cfg}
}

// config 返回启动时解析的有效配置（flags 解析结果，含 flag/env 覆盖）。
func (a *App) config() host.Config {
	return a.cfg
}

// StartHost 启动 host 会话（LocalHost 接口；Runner 内部自保护已运行检查）。
func (a *App) StartHost(cfg host.Config) error {
	return a.runner.StartHost(cfg)
}

// StopHost 停止 host 会话（LocalHost 接口）。
func (a *App) StopHost() {
	a.runner.StopHost()
}

// Running 报告 host 会话是否在运行（LocalHost 接口）。
func (a *App) Running() bool {
	return a.runner.Running()
}

// OpenPlatform 打开/聚焦平台窗口。
// 未绑定（无凭证）→ 直达设备管理页 {host}/hosts 引导绑定；已绑定 → 打开平台首页。
func (a *App) OpenPlatform(hostURL string) error {
	if strings.TrimSpace(hostURL) == "" {
		hostURL = a.config().Host
	}
	if a.config().Key == "" {
		hostURL = host.HostsURL(hostURL)
	}
	app := application.Get()
	// 拼 local_code 参数：平台页面据此建立本地通道（aic env.js 存 localStorage）
	if a.local != nil {
		sep := "?"
		if strings.Contains(hostURL, "?") {
			sep = "&"
		}
		hostURL = hostURL + sep + "local_code=" + url.QueryEscape(a.local.LocalCodeParam())
	}
	logv.Info().Msgf("opening platform: %s", hostURL)
	if w, ok := app.Window.Get("platform"); ok {
		w.SetURL(hostURL)
		w.Show()
		w.Focus()
		a.platform = w
		return nil
	}
	w := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:   "platform",
		Title:  "AIC Platform",
		URL:    hostURL,
		Width:  1280,
		Height: 800,
	})
	if w == nil {
		return fmt.Errorf("create platform window failed")
	}
	a.platform = w
	return nil
}

// PlatformWindow 返回平台窗口（常驻托盘模式：关闭=隐藏，托盘点击重新打开）。
func (a *App) PlatformWindow() application.Window {
	return a.platform
}

// openSettings 打开/聚焦设置窗口（本地页面，配置 host/key/work_dir/exec_timeout）。
func (a *App) openSettings() {
	app := application.Get()
	if app == nil || a.local == nil {
		return
	}
	if w, ok := app.Window.Get("settings"); ok {
		w.Show().Focus()
		return
	}
	w := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:   "settings",
		Title:  "AIC Desktop 设置",
		Width:  480,
		Height: 440,
	})
	w.SetHTML(settingsHTML(a.local.Port(), a.local.Code()))
	w.Show()
}

// settingsHTML 渲染设置窗口页面（注入本地 API 端口与 code，经 local_code 通道配置）。
func settingsHTML(port int, code string) string {
	return fmt.Sprintf(`<!doctype html>
<html><head><meta charset="utf-8"><title>AIC Desktop 设置</title>
<style>
body{font:13px -apple-system,sans-serif;background:#1e1e1e;color:#eee;margin:24px}
h3{margin:0 0 6px}
label{display:block;margin:10px 0 4px;color:#aaa}
input{width:100%%;box-sizing:border-box;padding:6px 8px;border:1px solid #444;border-radius:6px;background:#2a2a2a;color:#eee}
button{margin-top:18px;padding:8px 18px;border:none;border-radius:6px;background:#3b82f6;color:#fff;cursor:pointer}
button:hover{background:#2563eb}
#msg{margin-left:10px}
</style></head><body>
<h3>AIC Desktop 设置</h3>
<label>平台地址 (host)</label><input id="host" placeholder="https://ivec.ai">
<label>凭证 (key)</label><input id="key" placeholder="host_id.cred_ver.secret.uid">
<label>工作目录 (work_dir)</label><input id="work_dir" placeholder="系统临时目录">
<label>后台超时 (exec_timeout)</label><input id="exec_timeout" placeholder="30m">
<button onclick="save()">保存</button><span id="msg"></span>
<script>
const PORT = %d, CODE = %q;
async function call(name, body){
  const r = await fetch('http://127.0.0.1:'+PORT+'/api/'+name, {
    method: body===undefined?'GET':'POST',
    headers: {'x-aic-code': CODE, ...(body?{'Content-Type':'application/json'}:{})},
    body: body===undefined?undefined:JSON.stringify(body),
  });
  if(!r.ok){ const t = await r.text().catch(()=>''); throw new Error(t||('HTTP '+r.status)); }
  return r.status===204?null:r.json().catch(()=>null);
}
(async function(){
  try{
    const cfg = await call('get_config');
    document.getElementById('host').value = cfg.host||'';
    document.getElementById('key').value = cfg.key||'';
    document.getElementById('work_dir').value = cfg.work_dir||'';
    document.getElementById('exec_timeout').value = cfg.exec_timeout||'';
  }catch(e){ document.getElementById('msg').textContent = '加载失败: '+e.message; }
})();
async function save(){
  const msg = document.getElementById('msg');
  const host = document.getElementById('host').value.trim();
  const key = document.getElementById('key').value.trim();
  const work_dir = document.getElementById('work_dir').value.trim();
  const exec_timeout = document.getElementById('exec_timeout').value.trim();
  try{
    if(key){ await call('bind', {credential: key}); }
    await call('set_config', {host: host, work_dir: work_dir, exec_timeout: exec_timeout});
    msg.textContent = '已保存'; msg.style.color='#4ade80';
  }catch(e){ msg.textContent = '保存失败: '+e.message; msg.style.color='#f87171'; }
}
</script></body></html>`, port, code)
}
