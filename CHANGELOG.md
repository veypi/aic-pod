# Changelog

本仓库 0.x 阶段遵循 SemVer：破坏性变更进位 minor。
发版只改两处版本位——`cfg/config.go` 的 `Version`（带 `v` 前缀）与
`browser/manifest.json` 的 `version`（无前缀）；`desktop/package.json` 由
`make desktop-version` 从 `git describe` 自动同步。更早版本见 GitHub Releases。

## 未发布 — 2026-09-09

### 修复

- **Windows 桌面端 Alt+Space 弹系统菜单**（`desktop/main.js`）：Alt+Space 走系统
  DefWindowProc 弹窗口菜单（还原/最小化/最大化/关闭），且 WM_SYSKEYDOWN 被系统消费——
  页面收不到 keydown，应用内 launcher 快捷键（keymap 默认 leader=Alt + space）失效。
  现窗口聚焦期间 RegisterHotKey 抢占该组合（系统不再弹菜单），命中后把 Space 键回注
  平台页，走页面既有 keymap（改键位跟随）；失焦即注销；桌宠窗聚焦时仅吞掉。

### 新增

- **RTC 直连应答（2026-09-10，§6.4）**：新增 `libs/rtc`——owner 页面与设备的
  WebRTC DataChannel 直连（pion/webrtc v4 集成，纯 Go 无 cgo）。host 为纯应答方：
  `proto.Caps` 新增 `Mgmt{code,rtc}`（cfg `rtc` 开关，默认 true；关则不上报、
  不应答）；信令入向复用通配 inbox（`u.{uid}.h.host_{id}.rtc.in`，dispatch
  按后缀路由进 rtc 服务），出向 `u.{uid}.h.{id}.{ver}.rtc`；单 UDP mux（随机
  端口，防火墙友好）+ mDNS QueryOnly（解析浏览器 `.local` 化名候选）+
  60s 建连看门狗；DataChannel("fs") 鉴权帧（code 与本地管理 API x-aic-code
  同源，5 次失败锁 1 分钟）+ fs 帧协议（全 JSON 文本帧，32KB 内联阈值，
  48KB chunk 流式，8MB 发送高水位 backpressure）；fs 执行体复用 vcore.RunFS
  （granted=9 本地控制台信任级，fsauth 三域 deny/allow 照常生效）。
  环路对测 9 例（误码拒绝/鉴权+内联/64KB 流式结果重组/80KB 流式写注回/
  通道关闭 PC 回收/readbin 内联与流式精确往返、区间透传、错误帧）。
- **readbin 二进制字节出口（2026-09-10 同批）**：帧协议新增 `readbin` op
  （直连控制台私有，不属于 fs 指令集）——`vcore.ReadBin`（新，libs/vcore/
  readbin.go：字节区间读 + fsauth 同一判定实例 + 单次上限 256MB）+ base64
  文本帧回送（bin:true + attrs{mime,size,total}，超 32KB 走 chunk 流）；
  供前端预览/下载大二进制（视频等）。同批修复实网首测暴露的 P1：
  `vcore.Result` 补 json tag（缺 tag 时 Go 序列化出大写键 Content/Attrs，
  页面按小写解析静默丢空——Go 对测反序列化大小写不敏感、JS mock 用小写，
  双双漏检；rtc_test 加线上契约断言防回归）与 P2 加固（fs 通道关闭即回收
  PC，此前未 authed 连接永不回收；连接数上限 16 防信令面洪泛）。
- **桌面端主窗口 A/B 左右分区**（`desktop/main.js` + `electron-adapter.mjs`，新增
  `divider.html` / `divider-preload.js`）：A = 平台页常驻左侧；B 区（右）默认收起，
  AI browser 标签创建时自动展开（标签从全遮挡隐藏改为 B 区可见），最后一个标签关闭
  自动收起；「本地配置」从独立窗口改为内嵌 B 区设置视图（settingsView 保持 B 最前，
  新标签不顶掉正在编辑的设置页，关闭后露出标签或收起 B）。分隔条可拖拽调宽
  （B≥360 / A≥400，拖拽时覆层接管鼠标），宽度持久化 `userData/b-width.json`。
  adapter 新增 `tabControl.relayout`（resize/拖拽重设标签 bounds）与 `onTabsChanged`
  钩子，`hideAiTab` 修正 z 序（先重挂再置顶平台页，终态遮挡成立）。
  设置页卡片宽度响应式（`min(520px,100%)` + border-box）适配窄 B 区。
- **托盘「打开配置目录」**（`desktop/main.js`）：菜单新增项用系统文件管理器打开 Go
  后端配置根（`os.UserConfigDir()/aic`：config.yaml / aic.log / browser 状态同根）。
  Electron 侧按平台推导同一路径（darwin `~/Library/Application Support/aic`；
  win32 `%APPDATA%/aic`；linux `${XDG_CONFIG_HOME:-~/.config}/aic`），目录缺失时先建再开。
- **本地配置页三域授权编辑**（`ui/page/settings.html`）：fs / net / ssh 三域各一组
  policy 下拉（deny=仅允许名单放行 / open=除拒绝名单全放）+ deny / allow 名单编辑器
  （条目增删，回车或按钮添加）。保存随 set_config 全量提交九键（空数组 = 清空，匹配
  后端整体替换语义），条目形态校验由后端既有逻辑报错；卡片超高改为整页滚动（原 flex
  居中布局超高会截顶）。后端九键 API 此前已就绪，本次纯前端接入。
- **cua cursor 子命令**（`libs/host/cua.go` + `libs/vcore/{meta,levels}.go`）：`cua cursor on|off|state|motion|theme <id>` 控制 agent 光标浮层——驱动 MCP `set_agent_cursor_enabled` / `get_agent_cursor_state` / `set_agent_cursor_motion` / `set_agent_cursor_theme` 的 argv 面（motion 参数经未知 flag 透传）。背景：Windows 上浮层是覆盖整个虚拟屏的透明点击穿透分层窗口，cua 操作后残留导致全系统鼠标指针闪烁（cua-driver 0.25.0，同类症状 openai/codex#34340）；host 的 cua-driver MCP 子进程常驻复用、浮层不随操作销毁，`cua cursor off` 是无摩擦止血入口。
- **cua 声明基线 Write(2) → Read(1)**（`libs/vcore/meta.go` Decl）：声明层不设 Write 地板，读类子命令动态降级不再被 max(声明, 动态) 吃掉（对齐 git/json）；cursor 全子命令 = Read(1)。写/危险动作仍由 cuaRequired 动态提升。

### 测试

- 托盘配置目录 / 设置页授权区 / A/B 分区：`node --check desktop/{main,settings-preload,divider-preload}.js`
  绿；`vhtml check ui/page/settings.html` 绿；`go build ./...` 绿（ui embed 重编译通过）。
- Alt+Space 回注机制（隐藏窗口 + `webContents.sendInputEvent`）：页面收到 `{code:'Space', altKey:true}` 的 keydown/keyup（keymap 匹配条件），`globalShortcut.register('Alt+Space')` 合法返回 true。
- `TestMapCuaArgv`/`TestMapCuaArgvErrors`（cursor 成功/报错）、`TestCuaRequired`（cursor 全 Read）、`TestCuaDecl`（基线 Read）；`go build ./...` + `go vet ./libs/{host,vcore}` 绿；`go test ./libs/vcore ./libs/host` 除既有环境性失败（TestVcoreVectorsOnOSVFS 的 .env 用例在平台沙箱内 EPERM）外绿。

## v0.6.2 — 2026-09-09

### 修复

- **本地配置页保存被连接失败阻断**（`ui/page/settings.html`）：保存改为先落盘
  （set_config）→ 凭证改动过才 bind → 探测跳转；bind 失败不再中断保存（只提示
  “地址已保存；凭证未生效”），探测失败改中性提示，本地服务断开时提示重新打开
  配置页。修复换平台/改地址时“链接失败导致保存不了”。
- **桌面端换地址后新平台页无本地通道**（`desktop/main.js`）：注入白名单随
  `platform:open` 增量更新——改地址后新平台页恢复 `window.aicDesktop` 注入
  （绑定面板/窗口控制不再失效）。

## v0.6.1 — 2026-09-09

### 修复

- **Windows desktop 启动 ENOENT**：`Makefile` 的 `backend-bin` 用显式 `-o .../aic-backend`
  构建，而 go build 显式命名不会自动补 `.exe`（仅默认命名会）——Windows 包里是
  `resources/backend/aic-backend`，`main.js` 却固定 spawn `aic-backend.exe`，启动即
  `spawn ... ENOENT`（自 v0.5.4 Electron 迁移起存在）。现按宿主平台补扩展名，并新增
  Windows 打包后断言；顺带清理跨平台残留二进制（旧 Windows exe 曾被塞进 mac 包）。

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
