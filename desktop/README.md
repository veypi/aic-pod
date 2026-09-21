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
 ├─ browser-path.cjs：只注入 AIC_BROWSER_DEFAULT_PATH（独立 Chrome）
 │    Go libs/browser 经 pipe 自管 Chrome、profile、page、下载和输入租约
 │    browser/cua 经 hosts_tool 一次声明，由 hosts_rtc/1 与 hosts_nats/1 调用
 ├─ cua：Go libs/cua 自管 cua-driver MCP、窗口与快照
 │    scripts/sync-cua.mjs 将固定版本发行物同步到 vendor/cua → resources/cua
 │    main.js 注入 CUA_DRIVER_PATH/CUA_DRIVER_APP
 ├─ BaseWindow 主窗口：平台页 WebContentsView
 │    Browser viewer 通过 RTC 观看 Go 管理的 Chrome，接管后才能输入
 │    固定设备视口，viewer 关闭或缩放不影响页面生命周期
 ├─ worker 保活窗口（隐藏常驻，skipTaskbar）：加载 {平台根}/worker-keep.html——与平台页
 │    同源共享同一 nc SharedWorker 实例并持端口，平台页刷新（Cmd+R）不再销毁 worker/WS；
 │    崩溃原地重载、网络级失败 10s 重试（依赖平台先部署该静态页）
 ├─ 本地配置：独立设置窗口（系统边框 BrowserWindow，`app://aic` 协议加载
 │    settings-ui/；settings-preload 暴露设置桥 → IPC 'local:api' → 主进程 spawn 子命令；
 │    托盘「本地配置」直开，平台不可达首配时自动打开）——不依赖平台页/主窗状态；
 │    保存/绑定后自动重启后端子进程并原地重载设置页
 └─ 托盘 / 单实例 / 关闭=隐藏 / 桌宠（独立透明小窗）
```

窗口控制：壳页面经 preload（`window.aicDesktop`）调 IPC；`/api/window_*` 端点
保留（cli 浏览器壳下返回 desktop:false）。

Windows 热键：Alt+Space 由主进程在窗口聚焦期间 RegisterHotKey 抢占（否则走系统
DefWindowProc 弹窗口菜单、页面收不到 keydown），命中后 `sendInputEvent` 回注 Space
键到平台页，动作由页面 keymap 决定（默认 launcher）；失焦即注销。

浏览器分辨率：本地设置中的「浏览器分辨率」默认 1280×720，保存为
`browser_width` / `browser_height`（各 320–4096）。新建标签读取最新配置，已有标签不变。
修改 desktop 代码后需重启，并更新平台的 Browser 页面与 `os/browser-viewer.js`。

键盘焦点保留在平台 viewer 中，已有 leader/窗口快捷键先处理，普通输入与中文
composition 提交转发给离屏页面。原生内容的焦点交接已取消。

## 开发

```bash
# 1. 编译 Go 后端（desktop/bin/aic-backend）
make backend-bin
# 2. 安装依赖 + 启动（需独立 Chrome，或配置 browser_path）
cd desktop && npm install && npm start
```

平台页改动即时生效（远端 HTTP）；设置页（desktop/settings-ui/）与 main.js/preload.js
改动需重启 electron。设置页静态资源：vhtml 运行时 `settings-ui/vhtml/vhtml.min.js`
从 `../vhtml/dist/vhtml.min.js` 复制（升级 vhtml 后重新复制）。
browser 代码位于 libs/browser/，修改后重新编译 Go 后端；协议与测试见 [设备工具实现](../docs/hosts-tools.md)。Electron 不再包含 browser CDP 引擎。
内置 cua-driver（固定版本，见 desktop/cua.json）dev 下不自动下载——需要时手动
`npm run cua-sync`（→ vendor/cua，已 gitignore）；未同步时后端回落系统安装的 cua-driver。

独立 Chrome 开发时可用 `npm run browser-sync` 同步到 `vendor/browser/<platform>-<arch>`，
或者继续使用系统 Chrome / `browser_path`。同步清单在 `browser.json`。

## 打包（electron-builder，须在目标平台执行）

```bash
make desktop-darwin-arm64    # macOS arm64 → dist/aic-desktop-mac-arm64.dmg
make desktop-darwin-amd64    # macOS x64
make desktop-windows-amd64   # Windows → dist/aic-desktop-win-x64.exe（NSIS）
make desktop-linux-amd64     # Linux → dist/aic-desktop-linux-x64.AppImage
```

打包前自动同步 cua-driver（`desktop/cua.json` 固定版本 + sha256 校验 →
`vendor/cua → resources/cua`，三平台：mac `CuaDriver.app`、win/linux 裸二进制），
安装包自带 cua 能力，用户零安装。macOS 首次使用仍需用户在系统弹窗给 “Cua Driver”
授予辅助功能/屏幕录制（TCC 授权归上游 app 身份 com.trycua.driver，跨我们发版保持）。
升级 cua：改 `desktop/cua.json` 的 tag/sha256 后 `npm run cua-sync -- --force`。
受限网络（GitHub 直连不稳）：手动下载对应资产后 `npm run cua-sync -- --asset <文件>`
（仍走 sha256 校验）。

独立 Chrome 由 electron-builder 的 `beforePack` 自动同步（包括直接调用 electron-builder）。
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
