# Changelog

本仓库 0.x 阶段遵循 SemVer：破坏性变更进位 minor。
发版只改两处版本位——`cfg/config.go` 的 `Version`（带 `v` 前缀）与
`browser/manifest.json` 的 `version`（无前缀）；`desktop/package.json` 由
`make desktop-version` 从 `git describe` 自动同步。更早版本见 GitHub Releases。

## 未发布 — 2026-09-10

### 变更（破坏性）

- **OS 原生窗口内容 v2 反转模型**（`desktop/main.js` + `desktop/electron-adapter.mjs` +
  `desktop/remote-preload.js` + `desktop/README.md`；平台侧配套在 aic 仓
  `ui/os/wincontent.js` + `ui/layout/os.html`；设计唯一源 = aic/docs/os_native_windows.md）：
  平台页恒最顶且背景透明，原生内容（AI 标签池 / 设置 hostView）恒在其下（z 序不变量
  [tabs…, settings, platform]，标签池重排后经 onRestack 抬回）；可见性 = 页面整页 mask
  开洞（body mask-image SVG evenodd，「洞」= 内容区占位 rect），隐藏 = 撤洞 + 输入禁用，
  壳侧不再翻转 z 序（视图恒挂树，隐藏态 CDP 截图语义不变）。新增主进程输入路由：平台页
  `before-mouse-event` 命中洞 → preventDefault + 坐标翻译后 sendInputEvent 转发目标视图
  （含 sticky 拖拽捕获、mouseDown 焦点转移 wc.focus()）；wheel 不在该事件覆盖内
  （Electron 44 源码级依据）→ 页面 wheel listener → IPC `native:wheel` → 主进程命中复核
  + 符号换算（DOM deltaY 与 sendInputEvent 相反，deltaMode=1 按 40px/行）后转发。
  隔离实例真机自测（真 CGEvent 点击/键盘 + 渲染器级滚轮 + 独立 harness 页）：透明洞
  合成、洞内点击/拖拽/滚轮路由、洞外放行、焦点转移（打字进原生内容）、隐藏门控全部
  通过；平台侧新增 `buildHolesPath` 纯函数单测（8/8）。
- **修正（同日）——遮罩/弹窗不再触发内容隐藏**（`desktop/main.js` + `desktop/remote-preload.js` +
  aic 仓 `ui/os/wincontent.js` + `ui/page/local/browser.html` + `ui/page/local/desktop.html`）：
  v2 初版沿用 v1「遮挡即隐藏」语义（Alt/launcher/任何弹窗/浮窗出现 → 内容整隐）被废弃——
  反转模型的目的正是遮罩直接叠画。改为：① 洞只看结构条件（内容存在/rect 有效/页面可见），
  遮罩下内容保持可见（半透明可透出）；② 背景 mask 收敛到桌面背景层 `body::before`（内容
  元素背景透明 + 画布透明兜底；整页 body mask 取消，否则遮罩会被挖出洞）；③ 输入资格改
  页面侧判定（`elementsFromPoint`：洞内且栈顶为内容元素）→ `nativeWin.mouse / native:wheel`
  → 主进程复核 + 翻译下发（`before-mouse-event` 主进程路由废弃——壳无法感知 DOM 遮挡）；
  ④ 新增 `tracker.setEnabled`（0 标签空态撤洞）。真实实例复测：Alt/弹窗/浮窗覆盖下内容不
  隐藏，被盖处点击归遮罩、露出处点击进原生内容。

## 未发布 — 2026-09-09

### 变更（破坏性）

- **OS 原生窗口内容 P0：B 区/A/B 分区整体删除**（`desktop/main.js` +
  `desktop/electron-adapter.mjs` + `desktop/remote-preload.js` +
  `desktop/settings-preload.js` + `desktop/README.md`，删 `divider.html` /
  `divider-preload.js`；设计唯一源 = aic/docs/os_native_windows.md）：主窗口
  回到平台页恒满窗；AI browser 标签与「本地配置」设置视图（WebContentsView
  原生内容池）改由平台页 OS 窗口占位元素经新增 `window.aicDesktop.nativeWin`
  桥驱动贴位（rect/可见性唯一驱动源 = 渲染器；`native:state/tab-create/
  tab-close/tab-activate/tab-navigate/layout/host-layout` IPC 全走
  isPlatformFrame 白名单），隐藏 = z 序回落平台页之下（遮挡隐藏，隐藏态 CDP
  截图语义不变）。标签页 window.open 拦截转新标签（不再弹裸 BrowserWindow）；
  标题/URL/loading 经 `native:changed` 全量推平台页标签条。托盘「本地配置」：
  平台在线 → `native:open-host` → 平台 OS 开 `/local/desktop` 窗口贴位；
  平台不可达 → 主视图整窗加载本地设置页（remote-preload 本地分支给
  checkPlatform/openPlatform）。平台页整帧跳转/渲染进程崩溃 → 原生内容复位
  隐藏态。remote-preload 重构为平台白名单/本地 127.0.0.1 双分支。

### 修复

- **header 全屏按钮无效：创建窗口时显式传 `fullscreen: false` 毒化 NSWindow**（
  `desktop/main.js`）：macOS frameless BaseWindow 创建时传 `fullscreen: false`
  → 之后 `setFullScreen(true)` 恒 no-op（Electron 33.4.11 最小复现：显式 false
  2/2 失败、不传该键 2/2 正常）。改为只在启动即全屏时传 `fullscreen: true`。
  排查路径：渲染侧探针确认 IPC/白名单全通且 1.5s 延迟后 `isFullScreen()` 仍
  false → 同二进制隔离实验逐项二分（maximise/子 view/resize 监听/加载内容均
  无关）→ 唯一定位到创建选项。
- **remote-preload 本地分支误吞平台页**（`desktop/remote-preload.js` +
  `desktop/main.js`）：本地设置页判定原先按 hostname（`127.0.0.1|localhost`），
  平台开发态跑在 localhost:4000 时被误判为本地页——只暴露设置子集，窗口控制/
  nativeWin 全部缺失（症状：header 窗口按钮渲染但点击无效、AI 建标签不开窗、
  托盘本地配置无反应）。改为按主进程下发的精确 host:port（`allowed:hosts` 下发
  形态改 `{hosts, local}`）；`isLocalFrame` 同步收紧为 `127.0.0.1:<port>`。
- **视图菜单三个 role 在 BaseWindow 上崩溃**（`desktop/main.js`）：
  `role:reload/toggleDevTools/togglefullscreen` 找 `focusedWindow().webContents`，
  BaseWindow 无 webContents → undefined 异常（点「开发者工具」报错）。改为显式
  handler（platformView.reload / toggleDevTools / mainWin.setFullScreen）。

- **本地配置页：授权区样式丢失 + 操作会关闭页面**（`ui/page/settings.html` +
  `desktop/main.js` + `desktop/settings-preload.js`）：三域授权编辑器
  （renderAuth/listEl）是运行时 DOM，组件样式默认选择器只命中编译期节点
  （[vrefof]）→ 选择器统一加 `body ` 前缀走作用域穿透；「保存」不再跳转
  （只落盘/按需绑定/原地提示），「更新」改名「获取」且打开平台页不再关闭
  设置视图，删除「返回平台」（桌面 A/B 同窗后多余）。
- **Windows 桌面端 Alt+Space 弹系统菜单**（`desktop/main.js`）：Alt+Space 走系统
  DefWindowProc 弹窗口菜单（还原/最小化/最大化/关闭），且 WM_SYSKEYDOWN 被系统消费——
  页面收不到 keydown，应用内 launcher 快捷键（keymap 默认 leader=Alt + space）失效。
  现窗口聚焦期间 RegisterHotKey 抢占该组合（系统不再弹菜单），命中后把 Space 键回注
  平台页，走页面既有 keymap（改键位跟随）；失焦即注销；桌宠窗聚焦时仅吞掉。
- **设置页「关闭」按钮恒隐藏：setup 阶段碰 $refs**（`ui/page/settings.html`）：
  按钮显示逻辑原先写在 `<script setup>` 里 `$refs.btnClose.style.display = ''`——
  vhtml 契约 setup 在 DOM 编译前执行、`$refs` 尚不可用，该行失效 → 按钮恒
  display:none（hostView 内「关闭」从未可见；关闭链路 closeSettings → 销毁视图
  + host-closed → 关 OS 窗口因此从未生效）。改为声明式：setup 只放 `canClose`
  布尔标记（桥存在性判断），模板 `v-if` 渲染。

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
  环路对测 10 例（误码拒绝/鉴权+内联/64KB 流式结果重组/80KB 流式写注回/
  通道关闭 PC 回收/readbin 内联与流式精确往返、区间透传、错误帧、并发
  双路交错重组）。并发口径：响应/chunk 帧带请求 id 按 id 分流，设备端每请求
  独立 goroutine 执行、发送 sendMu 串行——readbin 页面侧并发（≤4 路信号量），
  fs 调用保持串行（2026-09-10 手机实网验证：华为浏览器 ArkWeb 直连建连 +
  4 路并发字节精确）。
- **readbin 二进制字节出口（2026-09-10 同批）**：帧协议新增 `readbin` op
  （直连控制台私有，不属于 fs 指令集）——`vcore.ReadBin`（新，libs/vcore/
  readbin.go：字节区间读 + fsauth 同一判定实例 + 单次上限 256MB）+ base64
  文本帧回送（bin:true + attrs{mime,size,total}，超 32KB 走 chunk 流）；
  供前端预览组件 SW 流式桥边下边播大二进制（视频/PDF 等）。同批修复实网首测暴露的 P1：
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

### 工具链：Electron 33.4.11 → 44.3.0（2026-09-10）

- **背景**：33 早已不在安全维护窗口（官方只回补最近 3 个大版本）且落后当前稳定线
  10 个大版本；升级到 44.3.0（Chromium 130 → 152 / Node 20 → 24），
  electron-builder 25.1.8 → 26.15.3。
- **API 迁移**：`session.setPreloads`（35 起废弃）→ `registerPreloadScript({type:'frame',
  id:'aic-remote-preload'})`；平台页 `setZoomMode('disabled')` 硬钉 zoom=1
  （nativeWin 坐标契约从“页面未启用 zoom”的约定改为框架保证）。
- **下载流变化（42+）**：`electron` 包不再 postinstall 下载二进制，改为首次运行/
  打包时按需下载（`npx install-electron --no` 可显式预下）；
  `ELECTRON_SKIP_BINARY_DOWNLOAD` 移除，镜像经 `ELECTRON_MIRROR` /
  `NPM_CONFIG_ELECTRON_MIRROR` 生效。
- **影响核对**：macOS ≥13（本机 26.6.2 ✓；CI macos-14/15 ✓）；32 位构建取消
  （只出 x64/arm64 ✓）；renderer `clipboard` 移除（平台用 web 面 ✓）；dialog
  `defaultPath` 默认 Downloads（只用 showErrorBox ✓）；Windows 全屏隐藏菜单
  （win 无应用菜单 ✓）；PDF OOPIF 与 ANGLE 静态链接列入回归面。
- **验证**：`node --check` 全绿；`go build ./...` 绿；`npx electron --version`
  = v44.3.0；GUI 冒烟通过（窗口控制/全屏、Alt+Space、托盘本地配置、AI browser
  建标签与隐藏标签截图）。

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
