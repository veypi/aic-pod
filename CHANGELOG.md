# Changelog

本仓库 0.x 阶段遵循 SemVer：破坏性变更进位 minor。
发版只改两处版本位——`cfg/config.go` 的 `Version`（带 `v` 前缀）与
`browser/manifest.json` 的 `version`（无前缀）；`desktop/package.json` 由
`make desktop-version` 从 `git describe` 自动同步。更早版本见 GitHub Releases。

## v0.6.0 — 2026-09-09

### 破坏性变更

- **移除 agent-browser 依赖**（`refactor(host)!`）：Go 侧删除外部 CLI 桥接与探测。
  browser 能力改由**壳 provider 机制**提供——desktop 经 Electron CDP 原生实现，
  插件与 desktop 共享同一份平台无关 core。

### 新增

- **三域授权模型**（fs / net / ssh × policy / deny / allow，`libs/fsauth` + `libs/netauth`）：
  每域默认立场 + 拒绝/允许列表（拒绝恒优先，显式允许可覆盖拒绝）、内置根、会话级临时
  grant 与 `--permanent` 落盘；`set_config` / `grant` 动态生效，无需重启。
- **exec 进程沙箱**（`libs/exec_procs`）：darwin `sandbox-exec`（Seatbelt）/ linux
  bubblewrap / windows 受限令牌 + 能力 SID ACL；按授予等级选 profile（read-only /
  workspace-write）、敏感环境变量清洗、资源限制、网络域出站闸；无可用后端 **fail-closed**。
  请求级 `nosandbox` 恒提升 Critical(4) 单独人工审批，审批通过（9）本身不豁免沙箱。
- **cua 一级命令**（desktop 本机 GUI 自动化，Go 原生桥接 cua-driver MCP）：
  感知 `apps`/`windows`/`snapshot`（AX 树 + `--png` 截图 + `--grep` 过滤）、动作
  `click`/`type`/`key`/`hotkey`/`scroll`/`drag`/`menu`/`set-value` 等、typed browser 家族
  （`bprepare`/`browser-state`/`navigate`/`bclick`/`btype`/`bend`）、`run` 脚本批处理
  （本地桥 + node runner）、输入法护栏（键盘动作自动切英文布局、非 ASCII 走剪贴板粘贴）、
  会话空闲结束后自动 `start_session` 重建并重试；`--delivery foreground` 与
  `--scope desktop` 提级 Danger(3) 逐次审批。
- **ssh / scp 一级工具**：独立目标闸（ssh 域 Policy），本机侧走 fsauth 门控。
- **desktop 内置 cua-driver 发行物**：`desktop/cua.json` 固定版本 + sha256，
  `scripts/sync-cua.mjs` 同步到 `vendor/cua → resources/cua`（macOS `CuaDriver.app`、
  win/linux 裸二进制 + UIA worker/cursor theme），`main.js` 注入 `CUA_DRIVER_PATH` /
  `CUA_DRIVER_APP`，用户零安装（macOS 首次仍需系统弹窗授予辅助功能/屏幕录制）。

### 变更

- browser 指令集 **core/adapter 拆分**：平台无关核心 `browser/src/tools/browser/core.js`
  + 插件 `chrome-adapter` / desktop `electron-adapter`（Electron CDP 原生接入）。
- desktop 主窗口改为隐藏「AI 工作区」标签页模型（browser 全程后台）；换新图标。
- 本地管理页 / 托盘细节调整（配置视图不暴露隐藏项）。

### 修复

- browser `get count` 返回匹配数量；`read` 截断改二分收刀（100K 上限）。
- desktop 壳通道 ESM 装配（`vendor/` 下 `type: module` 声明，避免 CJS 误解析）。

### 构建 / 发版

- 打包前自动 `cua-sync`（Makefile 目标 + npm `predist*` 钩子，sha256 校验，支持
  `--asset` 离线通道）。
- CI（`.github/workflows/build.yml`）：tag `v*` → desktop 全平台 + cli 全平台 +
  browser zip → `gh release create`（自动生成 release notes）。
