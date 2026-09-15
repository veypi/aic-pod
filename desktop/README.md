# AIC Desktop（Electron）

替代旧 wails3 壳：**Chromium 渲染 + Go 后端子进程**。渲染的是 HTTP 页面（本地壳
页面 + 平台页），渲染器零前端构建——Electron 只提供窗口/托盘/桌宠/窗口控制。

## 架构

```
Electron Main (Node, main.js)
 ├─ spawn bin/aic-backend（Go 二进制 = cli 编译产物：本地 vigo 服务 + NATS host 会话）
 │    └─ 握手：AIC_PORT_FILE 环境变量 → 后端写 {port, code} JSON
 ├─ browser 壳通道（browser-tool.mjs）：browser core（vendor/，与插件同源）
 │    + electron-adapter（webContents.debugger CDP）→ 127.0.0.1 TCP 换行 JSON
 │    → 向 Go 后端注册 provider（/api/provider/register），caps 出现 browser
 ├─ cua（本机 GUI 自动化）：Go 后端原生桥接 cua-driver（libs/host/cua.go，
 │    MCP 持久子进程懒启动）；启动探测到 cua-driver 二进制才声明（不经壳通道）；
 │    发行物内置：scripts/sync-cua.mjs 按 desktop/cua.json 固定版本 + sha256 同步到
 │    vendor/cua → resources/cua，main.js 注入 CUA_DRIVER_PATH/CUA_DRIVER_APP；
 │    macOS 走 CuaDriver.app daemon 唯一形态（TCC 授权归 com.trycua.driver，host 自动拉起）
 ├─ BaseWindow（frameless）主窗口：平台页恒满窗且恒最顶（背景透明，画面全部由页面
 │    自绘） + 原生内容池（OS 原生窗口内容 v2 反转模型，设计唯一源 =
 │    aic/docs/os_native_windows.md）——AI browser 标签（WebContentsView）
 │    恒在平台页之下，由平台页 OS 窗口占位元素经 nativeWin 桥
 │    驱动贴位；洞 = 内容区（背景层 mask + 内容元素透明；遮罩/弹窗直接叠画、不隐藏
 │    内容）；输入资格由页面侧 DOM 判定（elementsFromPoint）→ IPC
 │    `native:mouse / native:wheel` → 主进程复核 + 坐标翻译下发（含拖拽捕获 + 焦点
 │    转移）；原生内容聚焦时 leader 键由壳侧抓取（`native:leader / native:keys`，
 │    OS 布局快捷键保持可用，见下文「leader 键抓取」）
 ├─ worker 保活窗口（隐藏常驻，skipTaskbar）：加载 {平台根}/worker-keep.html——与平台页
 │    同源共享同一 nc SharedWorker 实例并持端口，平台页刷新（Cmd+R）不再销毁 worker/WS；
 │    崩溃原地重载、网络级失败 10s 重试（依赖平台先部署该静态页）
 ├─ 本地配置：独立设置窗口（系统边框 BrowserWindow，settings-preload；托盘「本地配置」
 │    直开，平台不可达首配时自动打开）——不依赖平台页/主窗状态；bind/unbind 成功后原地重载
 └─ 托盘 / 单实例 / 关闭=隐藏 / 桌宠（独立透明小窗）
```

窗口控制：壳页面经 preload（`window.aicDesktop`）调 IPC；`/api/window_*` 端点
保留（cli 浏览器壳下返回 desktop:false）。

Windows 热键：Alt+Space 由主进程在窗口聚焦期间 RegisterHotKey 抢占（否则走系统
DefWindowProc 弹窗口菜单、页面收不到 keydown），命中后 `sendInputEvent` 回注 Space
键到平台页，动作由页面 keymap 决定（默认 launcher）；失焦即注销。

leader 键抓取（设计 = aic/docs/os_native_windows.md §6）：原生内容
（AI 标签）持有键盘焦点时平台页收不到 keydown——壳对每个内容 view 挂
`before-input-event`（`leader-grab.js` 纯判定）：leader 集合精确命中 → 该键不进内容、
经 `native:keys` 转平台页合成 KeyboardEvent（复用页面 keymap/编排链路）并把键盘焦点
交接平台页；会话期间物理键全由平台页原生接收，leader 释放后焦点自动交还来源内容视图
（launcher 等经 `native:focus` 保留焦点）。leader 集合由页面经 `native:leader` 同步
（改键跟随，未同步 = 不抓取）。不做逐键转发的原因（Chromium 抑制 handled keyDown 后的
keyUp/char，壳侧观测不到释放）见 docs §6。

## 开发

```bash
# 1. 编译 Go 后端（desktop/bin/aic-backend）
make backend-bin
# 2. 安装依赖 + 启动（npm prestart 自动同步 browser 共享代码到 vendor/）
cd desktop && npm install && npm start
```

壳页面/平台页改动即时生效（HTTP 服务），main.js/preload.js 改动需重启 electron。
browser 共享 core 改动（browser/src/tools/browser/）经 npm prestart 同步，需重启 electron。
内置 cua-driver（固定版本，见 desktop/cua.json）dev 下不自动下载——需要时手动
`npm run cua-sync`（→ vendor/cua，已 gitignore）；未同步时后端回落系统安装的 cua-driver。

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

CI：`.github/workflows/build.yml` desktop job（tag v* 触发，五平台产物）。

## 发版

版本号：`package.json` version 与 `cfg.Version`（cli 兜底）保持一致；electron-builder
产物版本取自 package.json（Makefile desktop-version 目标自动同步 git 版本）。

## 已知差异（相对旧 wails3 壳）

- 渲染内核 WebKit → Chromium（首页/SPA 渲染性能对齐 Chrome）
- 桌宠为独立透明窗口（主窗口保持不透明，避免透明合成性能损耗）
- 拖动：CSS `-webkit-app-region: drag`（Chromium 原生），双击标题栏 mac 系统缩放
- 后端身份：AIC_DEVICE_TYPE=desktop 上报设备类型
