# AIC Desktop（Electron）

替代旧 wails3 壳：**Chromium 渲染 + Go 后端子进程**。渲染的是 HTTP 页面（本地壳
页面 + 平台页），渲染器零前端构建——Electron 只提供窗口/托盘/桌宠/窗口控制。

## 架构

```
Electron Main (Node, main.js)
 ├─ spawn bin/aic-backend（Go 二进制 = cli 编译产物：本地 vigo 服务 + NATS host 会话）
 │    └─ 握手：AIC_PORT_FILE 环境变量 → 后端写 {port, code} JSON
 ├─ BrowserWindow（frameless）加载 http://127.0.0.1:{port}/?code=xxx
 │    └─ 壳页面（aic-pod/ui：header + iframe 平台页）
 └─ 托盘 / 单实例 / 关闭=隐藏 / 桌宠（独立透明小窗 200×200）
```

窗口控制：壳页面经 preload（`window.aicDesktop`）调 IPC；`/api/window_*` 端点
保留（cli 浏览器壳下返回 desktop:false）。

## 开发

```bash
# 1. 编译 Go 后端（desktop/bin/aic-backend）
make desktop-backend
# 2. 安装依赖 + 启动（需本机 electron 环境）
cd desktop && npm install && npm start
```

壳页面/平台页改动即时生效（HTTP 服务），main.js/preload.js 改动需重启 electron。

## 打包（electron-builder，须在目标平台执行）

```bash
make desktop-darwin-arm64    # macOS arm64 → dist/aic-desktop-mac-arm64.dmg
make desktop-darwin-amd64    # macOS x64
make desktop-windows-amd64   # Windows → dist/aic-desktop-win-x64.exe（NSIS）
make desktop-linux-amd64     # Linux → dist/aic-desktop-linux-x64.AppImage
```

CI：`.github/workflows/build.yml` desktop job（tag v* 触发，五平台产物）。

## 发版

版本号：`package.json` version 与 `cfg.Version`（cli 兜底）保持一致；electron-builder
产物版本取自 package.json（Makefile desktop-version 目标自动同步 git 版本）。

## 已知差异（相对旧 wails3 壳）

- 渲染内核 WebKit → Chromium（首页/SPA 渲染性能对齐 Chrome）
- 桌宠为独立透明窗口（主窗口保持不透明，避免透明合成性能损耗）
- 拖动：CSS `-webkit-app-region: drag`（Chromium 原生），双击标题栏 mac 系统缩放
- 后端身份：AIC_DEVICE_TYPE=desktop 上报设备类型
