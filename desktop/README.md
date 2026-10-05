# AIC Desktop（Electron）

替代旧 wails3 壳：**Chromium 渲染 + Go 后端子进程**。渲染的是平台 HTTP 页面 +
本地设置页（自定义 `app://aic` 协议；**本地不监听任何端口**），渲染器零前端构建——
Electron 只提供窗口/托盘/桌宠/窗口控制。

## 架构

```
Electron Main (Node, main.js)
 ├─ spawn bin/aic-backend（Go 二进制 = cli 编译产物：NATS host 会话）
 │    └─ 设置/凭证：spawn `aic-backend config get|set / bind / unbind` 子命令读写
 │       config.yaml（stdin JSON/凭证）——无端口握手、无校验码；保存后重启子进程生效
 ├─ browser/cua：Go MCP manager 直接启动官方 agent-browser mcp / cua-driver mcp
 │    MCP 工具与 schema 原样暴露，状态归上游服务
 ├─ resources/agent-browser：官方原生二进制（AIC_AGENT_BROWSER_BUNDLE_DIR）
 ├─ resources/browser：Chrome for Testing（AIC_BROWSER_BUNDLE_DIR）
 ├─ resources/cua：官方 CuaDriver（AIC_CUA_BUNDLE_DIR）
 ├─ BaseWindow 主窗口：平台页 WebContentsView
 │    Browser UI 经 RTC command 调用上游工具，同一 RTC 连接转发上游画面和人工输入
 ├─ worker 保活窗口（隐藏常驻，skipTaskbar）：加载 {平台根}/worker-keep.html——与平台页
 │    同源共享同一 nc SharedWorker 实例并持端口，平台页刷新（Cmd+R）不再销毁 worker/WS；
 │    崩溃原地重载、网络级失败 10s 重试（依赖平台先部署该静态页）
 ├─ 本地配置：独立设置窗口（系统边框 BrowserWindow，`app://aic` 协议加载单文件静态页
 │    settings-ui/settings.html；settings-preload 暴露设置桥 → IPC 'local:api' → 主进程 spawn
 │    子命令；托盘「本地配置」直开，平台不可达首配时自动打开）——不依赖平台页/主窗状态；
 │    保存/绑定后自动重启后端子进程并原地重载设置页
 └─ 托盘 / 单实例 / 关闭=隐藏 / 桌宠（独立透明小窗）
```

窗口控制：平台页经 preload（`window.aicDesktop`）调 IPC（minimise/maximise/close/
fullscreen/pet/restore）；外链经 `openExternal` 交系统浏览器；普通浏览器无此能力。

Windows 热键：Alt+Space 由主进程在窗口聚焦期间 RegisterHotKey 抢占（否则走系统
DefWindowProc 弹窗口菜单、页面收不到 keydown），命中后 `sendInputEvent` 回注 Space
键到平台页，动作由页面 keymap 决定（默认 launcher）；失焦即注销。

浏览器工具、tabId、元素引用 和参数均遵循上游。查看实际 schema 使用
`mcp tools browser` / `mcp describe browser <tool>`。实时 UI 复用 agent-browser 的原生流，由 Pod 转发；没有另一条 CDP 连接或自研 Browser MCP。

## 开发

```bash
# 1. 编译 Go 后端（desktop/bin/aic-backend）
make backend-bin
# 2. 安装开发依赖与固定版本的上游运行依赖
cd desktop && npm install && npm run runtime-sync
npm start
```

平台页改动即时生效（远端 HTTP）；设置页（desktop/settings-ui/settings.html 单文件静态页，
无框架/无构建、数据全走 IPC 桥）与 main.js/preload.js 改动需重启 electron。
browser/CUA 使用上游独立程序，没有自研 server 或工具适配层。
`mcp.servers` 可完整替换默认配置或添加第三方服务。协议见 [设备 MCP](../docs/hosts-tools.md)。
开发依赖分别由 `npm run agent-browser-sync`、`npm run browser-sync`、`npm run cua-sync` 同步。
版本在 `agent-browser.json`、`browser.json`、`cua.json` 固定。

## 打包（electron-builder，须在目标平台执行）

```bash
make desktop-darwin-arm64    # macOS arm64 → dist/aic-desktop-mac-arm64.dmg
make desktop-darwin-amd64    # macOS x64
make desktop-windows-amd64   # Windows → dist/aic-desktop-win-x64.exe（NSIS）
make desktop-linux-amd64     # Linux → dist/aic-desktop-linux-x64.AppImage
```

打包前自动同步 cua-driver（`desktop/cua.json` 固定版本 + sha256 校验 →
`vendor/cua → resources/cua`，三平台：mac `CuaDriver.app`、win/linux 裸二进制），
安装包直接包含上游的驱动与 MCP 模式。macOS 先把 `resources/cua/darwin/CuaDriver.app`
安装到 `/Applications/CuaDriver.app`，遵循上游按应用名启动 daemon 的布局；升级后执行
官方 `cua-driver stop` 停止旧 daemon。首次使用仍需用户在系统弹窗给 “Cua Driver”
授予辅助功能/屏幕录制（TCC 授权归上游 app 身份 com.trycua.driver，跨我们发版保持）。
升级 cua：改 `desktop/cua.json` 的 tag/sha256 后 `npm run cua-sync -- --force`。
受限网络（GitHub 直连不稳）：手动下载对应资产后 `npm run cua-sync -- --asset <文件>`
（仍走 sha256 校验）。

独立 Chrome 和 agent-browser 由 electron-builder 的 `beforePack` 自动同步（包括直接调用 electron-builder）。
`browser.json` 固定 [Chrome for Testing](https://github.com/GoogleChromeLabs/chrome-for-testing)
版本及 macOS arm64/x64、Windows x64、Linux x64 官方归档 SHA-256；校验成功后才替换缓存。
完整资源和随附声明位于 `resources/browser/<platform>-<arch>`，只复制当前架构，不进入 asar。
`afterPack` 与 `check-asar.mjs` 校验版本标记、可执行文件和必要资源；缺失即构建失败。
升级时一起更新版本和四个哈希，再 `npm run browser-sync -- --force`。
离线归档可用 `npm run browser-sync -- --asset <chrome-平台.zip>`，仍严格校验哈希。

CI：`.github/workflows/build.yml` desktop job（tag v* 触发，四目标产物）。

## 发版

版本号：`package.json` version 与 `cfg.Version`（cli 兜底）保持一致；electron-builder
产物版本取自 package.json（Makefile desktop-version 目标自动同步 git 版本）。

## 已知差异（相对旧 wails3 壳）

- 渲染内核 WebKit → Chromium（首页/SPA 渲染性能对齐 Chrome）
- 桌宠为独立透明窗口（主窗口保持不透明，避免透明合成性能损耗）
- 拖动：CSS `-webkit-app-region: drag`（Chromium 原生），双击标题栏 mac 系统缩放
- 后端身份：AIC_DEVICE_TYPE=desktop 上报设备类型
