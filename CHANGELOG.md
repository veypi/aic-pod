# Changelog

本仓库 0.x 阶段遵循 SemVer：破坏性变更进位 minor。
发版改 `cfg/config.go` 的 `Version`（带 `v` 前缀）并在本文新增版本节；
`desktop/package.json` 由 `make desktop-version` 从 `git describe` 自动同步。
更早版本见 GitHub Releases。

## 未发布

- 修复 fs 写便利逻辑把写授权放大到已存在祖先的 bug：write/curl -o/writebin/cp/mv 补父目录与 mkdir -p 的存在性探测不再做策略门控，写检查只覆盖实际创建的层级（先检后建、拒绝时零副作用）；只授权单个文件即可写入，与 edit/mkdir/remove「谁被创建/修改就查谁」同口径。
- browser/cua 改为 hosts_tools/1 typed 方法声明，RTC 和 NATS 共用鉴权与调用分发器；宿主不再托管其业务 session/operation/resource。
- browser 迁入 Go，自管独立 Chrome 与 pipe CDP；删除 Electron browser 引擎、TCP provider 和旧调用入口。Desktop 只注入 Chrome 默认路径。
- cua 驱动、窗口、快照状态迁入独立工具。前端 browser viewer 改为只读帧流和显式输入接管。
- 当前协议、配置、测试和范围见 [hosts-tools.md](docs/hosts-tools.md)。文件管理与普通 exec 本次不迁移。
- 移除会话授权撤销与断线清空：删除 `authorization.revoke` 动作、origin 拉黑窗口与 NATS 断线自清；临时授权改为纯进程内存态（按 Session 隔离，仅随进程退出/重启清空），无调用方的 `fsauth/netauth.ResetTemporary` 一并删除。

## v0.7.0 — 2026-09-20

### 破坏性变更

- **RTC 与服务器代理传输改走共享命令运行时（旧通道与 caps 上报不兼容）**
  （`protocol/hosts`（新）+ `libs/rtc` 重构 + `libs/host`）：RTC 拆分为
  hosts-control / hosts-data / hosts-live 三条数据通道，移除 `fschan` 私有
  文件通道；caps 按 rtc/proxy 分别上报协议与命令元数据，不再上报 mgmt code；
  新增代理请求主题 `u.{uid}.h.host_{id}.proxy.req`。依赖旧 caps 形态或 RTC
  文件通道的消费方需同步升级。

- **桌面端窗口呈现架构替换：WebContentsView 池 → 固定视口离屏窗口**
  （`desktop/electron-adapter.mjs`、`desktop/main.js`）：浏览器标签改为隐藏
  离屏窗口（DPR=1、后台不节流）渲染，JPEG 帧经 hosts/1 live 流推给平台页
  （ack 背压）；移除窗口层叠重排与 leader 键抓取（leader 会话交接行为不再
  存在）；页面输入统一为 `native:mouse/wheel/key/text/edit/reset` IPC。

### 新增

- **hosts/1 与 fs/1 线协议**（`protocol/hosts`（新）+ `protocol/fs`（新））：
  统一信封与错误模型（code/message/details/retry/effect）、准入 ticket
  （direct/proxy 分离签名、生命周期与时钟偏差约束、重放防护）、二进制流分帧
  与固定测试向量；fs/1 文件方法与结果 schema 由 RTC 与代理两种传输共用。

- **host 共享命令运行时与 fs/1 provider**（`libs/hostcmd`（新）+ `libs/hostfs`（新））：
  provider 目录与会话（resume token）、操作去重/查询/取消、有界执行与每会话
  配额；RTC/代理共用字节传输状态机（bytes.create/seal/read、seq/offset ack、
  重复块检测）；hostfs 提供 roots/home/stat/list/read/write/mkdir/remove/find/
  copy/move（条件写、原子替换、平台原生 move）。

- **桌面端浏览器窗口经 hosts/1 live 流呈现**（`desktop/browser/direct.mjs`、
  `input-queue.mjs`、`live-socket.mjs`（新））：browser 命令直连 dispatch；
  连续 move/wheel 输入合并推送；固定视口离屏页 JPEG 帧推送。

- **浏览器视口设置**（`cfg` + `api` + `ui/page/settings.html`）：`browser_width`
  / `browser_height`（默认 1280×720，范围 320–4096，越界回落），本地设置页
  可录入；旧客户端缺省字段保持原值。

- **新配置项**（`cfg`）：`hosts_upload_bytes` / `hosts_proxy_upload_bytes` /
  `hosts_streams`（零值使用设备默认 512 MiB / 64 MiB / 4）。

### 变更

- RTC/代理/命令服务装配到共享运行时（`libs/host` 适配器 + `libs/proto` +
  `api`）：provider 注册 Direct 通道；会话结束释放 UI/驱动绑定。
- RTC 实现重写（`libs/rtc/rtc.go`、`peer.go`、`live.go`；删除 `fschan.go`）。
- 桌面端（`electron-adapter.mjs`、`main.js`、`remote-preload.js`）：离屏窗口
  生命周期与视口归一（contain-fit 含入、显示坐标映射）；`native:frame` 转发；
  移除 `leader-grab.js`（含单测）。
- 文档：新增 `docs/hosts-direct-protocol-proposal.md`（直接通道设计记录）；
  `desktop/README.md` 更新离屏固定视口架构；`docs/design.md` 同步。

### 修复

- **proxy 票据按签发时间校验生命周期，容忍时钟偏差**（`protocol/hosts/proxy.go`）：
  签发窗口由签发者给定（issued_at + TTL 60s），接收方验证容忍 ±5s 未来签发
  且不延长过期；零/负/超长生命周期与超容差未来签发一律拒绝；配套用例覆盖
  偏差边界、合法 MAC 下窗口篡改，以及重发与断连后重放（`libs/hostcmd/access_test.go`）。

### 测试

- 协议向量与运行时单测扩充：hosts/1（ticket/stream 固定向量）、fs/1、
  hostcmd（runtime/access/transfer/live）、hostfs（fs/operations/copy）、rtc
  跨通道、browser（direct/input-queue/live-socket/viewport 等）。
- desktop `npm test` 113 例；live 回归新增固定视口/后台渲染/隐藏渲染用例
  （真 Chromium CDP）。

## v0.6.7 — 2026-09-18

### 破坏性变更

- **Chrome 扩展产品整体移除**：删除 `browser/`（MV3 扩展源码、options/popup、
  扩展端 PageFS/fsops、SDK 与 tools）及全部构建、分发与同步入口——Makefile
  `build-browser`、CI browser job（Release 不再产出 `aic-browser.zip`）、
  `desktop/scripts/sync-browser.mjs` 与旧 `vendor/browser` 打包路径；browser
  能力今后仅由桌面端提供，desktop 不再从扩展同步共享代码。

- **browser / cua 命令全面替换为 ui/1（不兼容旧用法）**：命令、返回、等级与
  错误语义整体重写，命令与等级的唯一来源 = `protocol/ui/schema.json`（Go/JS
  双端同读）。旧子命令与返回格式、裸 `@eN`、cua driver 参数透传与旧 `cua run`
  runner（`libs/host/cua_run.go` + `cua_runner.mjs`）均不兼容；旧 cua 输入法/
  键码机制（`cua_ime*`）一并删除。打包校验显式拒绝旧扩展代码路径。

### 新增

- **ui/1 统一交互协议（browser 与 cua 共用）**（`protocol/ui`（新）+
  `desktop/ui/protocol.mjs`（新）+ `libs/host/{ui_script,ui_response,cua_ui}.go`
  （新）+ `libs/vcore`）：统一目标模型（`target list/use/current`，绑定按会话
  隔离）、带快照身份的 ref（`@s…:eN`，过期返回 `stale_ref` 不自动换节点）、
  语义 locator（role/name、label、css、`--at` 图片坐标）与动作集
  （`open/navigate/snapshot/screenshot/read/get/click/fill/type/press/scroll/move/
  drag/set/wait/dialog/upload/download/eval` 等）；统一 UiResult——content
  默认文本分块（`[ui/1]` / `[target]` / `[action]` / `[observation]` /
  `[data]` / `[artifacts]` / `[warnings]` / `[error]`），`--format json` 同构；
  默认正文预算 12 KiB，完整结果落会话 `.ui/`，图片经 image_data/落盘通道。
  动作与观察分离：观察失败保留动作结果（`observation_failed`），效果无法确认
  返回 `effect_unverified`，断连时 `performed=unknown` 且不自动重试。

- **browser 端独立实现**（`desktop/browser/`（新）+ `desktop/browser-tool.mjs` +
  `desktop/electron-adapter.mjs`）：直接驱动 Electron CDP；network/console
  有界采集；`dialog accept/dismiss`（同步弹窗立即返回 `dialog_open` 与
  `action.performed=unknown`，控制命令返回 `browser_busy`，处理后续行结果在
  `data.resumed`，不自动接受/取消、不重放输入）；upload/download 经 host 文件
  授权；`eval` 运行于页面上下文（Danger）。

- **cua 端**：driver 会话过期自动恢复（当次 `session_expired` 不重放动作，下一
  条命令经同一 MCP 子进程 `start_session` 恢复，旧 target/ref 全部失效）；
  `apps/open/menu/window bounds/activate/clipboard/cursor/doctor`；原生 AX 与
  图片坐标两条定位路径，输入效果无法确认时返回 `effect_unverified`。

- **批量脚本 `run --code/--file`**（`libs/uiscript`（新）+ `cli` worker 入口）：
  browser/cua 注入同一 `ui` API（无 Node/require/fetch/setTimeout，延时用
  `ui.wait`）；由 host 以一次性纯 Go JS 解释器（goja）子进程执行，经 OS 只读/
  禁网沙箱启动（`nosandbox` 不豁免）；脚本与文件 ≤512 KiB、整个 run 要求
  Danger(3)、每步复用单命令调度与权限；限额：并发 4 worker、单脚本 256 步、
  日志 64 KiB、return 1 MiB、累计步骤结果 8 MiB；错误带 `code/step/result`，
  步骤结果记录在 `data.steps`。

- **持久执行日志**：host 按 session_id/msg_id 记录已授权 UI 请求——并发重发
  不重复执行，完成结果重放缓存，崩溃遗留 pending 返回 `outcome_unknown`，同
  ID 不同参数返回 `request_conflict`。

### 变更

- `libs/vcore` browser/cua 命令元数据与分级重写为 ui/1（Read 观察、Write 交互、
  Danger 用于 run/eval/activate/foreground）；`api` provider 注册增加 EndSession
  通道（会话结束释放 UI 绑定与驱动会话）；cli 增加隐藏 worker 子命令入口。
- 构建与 CI：Makefile 移除 browser 目标，release 仅含 cli+desktop；CI 在打包前
  运行 `go test ./protocol/ui` 与 `npm test --prefix desktop`；`check-asar` 改为
  递归校验入口模块的相对 require/import，并校验 `ui/schema.json` 内容。
- 打包：`electron-builder.yml` files 白名单更新（`browser/**`、`ui/**`；
  `protocol/ui/schema.json` 复制为 asar 内 `ui/schema.json`）；`desktop/package.json`
  移除 sync-browser 钩子，新增 `test` / `test:browser:live` 脚本。
- 文档：新增 `docs/ui-protocol.md`（运行时支持清单与验证入口）与
  `docs/ui-protocol-proposal.md`（设计记录）；README / desktop/README /
  design.md / host_sandbox.md 同步更新；删除 `docs/browser-client.md`。

### 测试

- 跨语言固定验收向量（`protocol/ui/testdata/cases.json`）由 Go 与 JS 解析器
  同跑；`npm test`（98 例）覆盖协议解析、结果渲染与 run 边界；live 验收覆盖
  真 Chromium CDP（输入、状态、ref 失效、截图、文件、网络/控制台、同步/嵌套/
  异步弹窗、query 与滚动）与 macOS 原生夹具（含会话恢复与旧 target 拒绝）。

## v0.6.6 — 2026-09-16

### 变更

- **四域统一 deny 优先，执行端策略不接受审批提权（行为变更）**（`libs/policy`（新）+
  `cfg` + `libs/{fsauth,netauth,host,exec_procs,vcore}` + `api`；`docs/host_sandbox.md`
  重写为执行策略与原生沙箱现状说明）：新增 `libs/policy` 共享原语包（fs_allow 条目
  解析含 `ro:` 只读前缀、exec/net 条目校验、`CommandAllowed`），cfg/fsauth/netauth/
  host 单源引用；cfg 新增 exec 域三键（`exec_policy`/`exec_deny`/`exec_allow`，默认
  open）+ `ValidateAuth` 在 Load/Save/api.SetConfig 前显式校验（坏配置不替换生效
  快照）+ `LockUpdate` 串行化本地配置读改写；判定收敛为 deny 恒优先（删除「具体度
  优先 / allow 覆盖 deny」——netauth 端口具体度、fsauth allowHit 覆盖、沙箱
  DenyOverride 三处同期移除），fs_policy=deny 下未命中 allow 的路径读写双拒（不再
  回落 3 级审批）；fsauth 新增只读授权（fs_allow `ro:` 前缀）与 RuntimeReadRoots/
  SystemCAReadPatterns 并入读范围、fs_deny 裸路径展开子树；host 新增 exec 域 grant
  （`grant exec <cmd> [--temp|--permanent]`）与会话授权生命周期（`_session_end`/
  NATS 断线/退出清临时授权）；执行端不再发起审批（waiting/NeedApproval 一律收敛
  rejected）；沙箱 fail-closed（沙箱路径）：planConfined 前置 validateProcessPolicy；
  nosandbox 免沙箱执行不再叠加本地 fs/net 策略校验——请求级经 Critical(4) 审批
  （granted 9 随签名下发）即执行，ssh/scp 内部管控调用与全局 no_sandbox 配置同属
  免沙箱来源；darwin
  seatbelt 默认 deny file-read* 后按 allow 显式放行、linux bwrap 改空根+只读白名单
  bind（工作区不再自动成为可写根）；vcore git 子命令分级补 branch（-d/-D/-m/-M/
  -c/-C/-f 破坏性标志为危险）。

- **浏览器扩展执行策略与 session grant**（`browser/src/sdk/execution_policy.js`（新）+
  `client.js` + `page_fs.js` + `background.js`）：扩展端 `ExecutionPolicy` 校验
  fs/exec 的 policy/deny/allow 条目（`ro:` 前缀、命令名形态；pathMatch 支持
  `*`/`**`/`?` 通配与路径归一），PageFS 读写前经 `checkFs` 判定；`AICClient` 内置
  `grant` 命令（fs|exec；`--temp` 会话内 / `--permanent` 经 `opts.saveExecutionPolicy`
  落盘并更新当前策略），会话授权按 sid 存放、断线/close/`_session_end` 清空；fs/exec
  未授权请求直接 error 与提示（不再回 waiting/need_approval），`_respond` 兜底把
  waiting 归一为 rejected（删除审批字段）；background.js 接线 `settings.executionPolicy`
  与 `saveExecutionPolicy` 写回。

## v0.6.5 — 2026-09-15

### 变更

- **本地配置回归独立窗口：不再依赖平台页/主窗状态**（`desktop/main.js` +
  `desktop/remote-preload.js` + `desktop/settings-preload.js` +
  `desktop/electron-adapter.mjs`；平台侧配套在 aic 仓 `ui/layout/os.html` +
  `ui/page/local/`；设计唯一源 = aic/docs/os_native_windows.md）：此前「本地配置」在
  主窗口内以 hostView 贴位渲染——托盘入口在平台页在线时经 `native:open-host` 通知
  平台页开 `/local/desktop` OS 窗口（配置可见性挂在平台页窗口模型上），平台不可达时
  整窗导航到本地设置页；平台页卡死/异常时配置进不去。改为独立设置窗口
  （BrowserWindow，系统边框，独立 partition + settings-preload，单例、关闭即销毁）：
  托盘「本地配置」直开，平台不可达首配时自动弹出（主窗停留 loading 提示）；不依赖
  平台页/主窗状态，主服务出问题也能改基本配置。同时删除主窗内 hostView 全链路：
  `applyHostLayout / ensureSettingsView / detachSettingsView / destroySettingsView /
  settingsInputTarget / clampRect`、`native:host-layout` IPC、
  `native:open-host / native:host-closed` 通知、z 序不变量收敛为 [tabs…, platformView]；
  `remote-preload` 删本地设置页分支与 `hostLayout/onOpenHost/onHostClosed`
  （`allowed:hosts` 只下发平台 host 数组）；`settings:close` 变为关窗口。设置页
  「获取」（探测 + 主窗跳 /hosts、配置窗保留）与「关闭」按钮行为不变。

- **windows 盘符虚拟根 + 路径归一收口（行为变更）**（`libs/host/osvfs.go` 重设计 +
  `libs/proto/path.go` + `libs/vcore/{env,ls,rg}.go` + `libs/fsauth/fsauth.go` +
  `libs/host/dispatch.go`；设计 = aic/docs/instruction_sets_v2.md §2.1.1 盘符路径一节，
  实施记录 = docs/host_sandbox.md §13）：win host 的 `/` 从「当前盘根」改为虚拟挂载
  根（`ls /` = 盘符列表、不递归；`rg /` 拒绝；`/` 上文件操作与非盘符绝对路径报错）；
  规范形 `C:/…`（盘符字母大写，裸 `C:` = 盘符根）；输入容错归一（`C:\…`、
  `/C:/…`、`//C:` 多斜杠前缀 → `C:/…`）收口于 `proto.NormalizeDrivePath` 单一纯函数
  （`ResolvePath` 主入口 + `OSVFS.winToOS` 执行层兜底共用——绝对路径先 `path.Clean`
  折叠多斜杠再判盘符形，归一结果中 `//C:`、`/C:` 形态不存在）；`rm /`/`mv /`
  与盘符根全部命中根保护（补 `rm /` 删当前盘根漏洞；`//C:` 多斜杠曾绕过等值比较——
  规则层见 `/C:`、执行层见 `C:\`，归一共用后消除）；`fsauth.canonical` 裸盘符按盘根
  展开。测试：proto 向量（含多斜杠前缀）/ winToOS 纯函数向量 / vcore 虚拟根行为 /
  fsauth 裸盘符 / `rm /`、`//C:` 绕过回归向量；darwin 全绿 + windows/linux 交叉编译
  通过，真机行为待 win 验证。

- **worker 保活窗口 + 本地配置窗原地重载**（`desktop/main.js`；平台侧配套 = aic 仓
  `ui/worker-keep.html`，go:embed——**发布顺序：先平台后桌面**，否则保活页 404 静默
  失效）：平台页是 nc SharedWorker 的唯一客户端，平台页刷新（Cmd+R）会销毁 worker/WS
  使整条 nc 通道冷启动；新增隐藏常驻窗口 `keepWorkerAlive` 加载平台根路径
  `/worker-keep.html`（同 URL 共享同一 SharedWorker 实例并持端口），worker/WS 跨平台页
  刷新保持存活；渲染进程崩溃原地重载、网络级失败 10s 重试（重试前比对当前期望地址，
  防切换平台后旧定时器拉回旧地址）。bind/unbind 成功后 `reloadSettingsIfOpen` 原地重载
  本地配置窗（展示 pod 侧最新凭证/连接状态）。

- **默认平台地址迁移至 ivec-ai.com**（`cfg/config.go` 默认值 + `Dockerfile` + `README` +
  浏览器扩展设置页 + browser SDK 兜底；本地 API CORS 信任名单与 desktop 平台白名单
  保留旧域 `ivec.ai` 兼容）：WS 握手不跟 301（旧域 301 重定向会使 NATS wss 握手报
  `invalid websocket connection`），默认 host 必须直连 `ivec-ai.com`；纯默认值替换，
  不做旧配置迁移。

## v0.6.4 — 2026-09-14

### 修复

- **桌面端打包补全：leader-grab.js 白名单 / vendor/browser 同步 / asar 校验**（`desktop/electron-builder.yml` +
  `Makefile` + `desktop/scripts/check-asar.mjs`（新））：v0.6.3 桌面包两处遗漏——① files 白名单漏了新增的
  `leader-grab.js`（main.js 启动即 `require('./leader-grab')`，缺失则主进程崩溃）；② Makefile 的 `desktop-*`
  只跑 cua-sync、未跑 sync-browser（npm predist 钩子对 CI 直调 electron-builder 不生效），`vendor/browser`
  未进包（`browser-tool.mjs` 静态 import 其 core，桌面端 browser 能力注册失败）。修复：白名单补
  `leader-grab.js`；新增 `browser-sync` 目标并接入三平台打包；打包后 `check-asar.mjs` 校验各入口相对
  require/import 均存在于 asar + `resources/backend` 后端二进制存在，不通过即构建失败。本地全链路验证：
  asar 含 `leader-grab.js` 与 `vendor/**`、校验通过。
- **check-asar.mjs 兼容 Windows 反斜杠路径**（`desktop/scripts/check-asar.mjs`）：@electron/asar 的
  listFiles 以 `path.join` 构建条目（Windows `\` 分隔、posix `/`），归一化统一为 posix `/` 形式 +
  空集合防护（此前 Windows 打包后校验误报「入口未进包」，其余平台正常）。

### 变更

- **macOS 包 ad-hoc 深签**（`desktop/scripts/after-pack-adhoc-sign.js`（新）+ `desktop/electron-builder.yml`）：
  `identity: null` 跳过后由 afterPack 钩子对 .app 做 `codesign --force --deep --sign -`（绕开 electron-builder 26
  直接 ad-hoc 的相机/麦克风失效坑，electron-builder#9529）。效果（本地 quarantine 模拟实测）：下载打开提示从
  「已损坏，无法打开」（只能 xattr 清隔离）变为「未打开——Apple 无法验证」（点「完成」后在 系统设置 →
  隐私与安全性 → 仍要打开 放行）。彻底免放行仍需 Developer ID 签名 + 公证。

> 注：v0.6.3 的桌面安装包（mac/win/linux）因上述遗漏不可用（启动崩溃），请改用 v0.6.4。

## v0.6.3 — 2026-09-14

### 破坏性变更

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
- **修正：桌面/设置窗口「一片白」——背景层未被挖洞**（aic 仓 `ui/layout/os.html` +
  `ui/os/wincontent.js`）：v2 修正版将整页 mask 收敛到桌面背景层时，背景层仍写作
  `body::before`——但平台框架会把布局样式作用域重写（`body`→`[vref="/layout/os"]`），
  运行时注入的 `body::before` mask 规则打不中真实伪元素；且 vhtml 会把页面 body 背景
  落在路由容器 `div[vsrc]` 上，该层也无处被挖 → 两处浅色层盖住洞（表现为「一片白」）。
  修复：①桌面背景改为布局层真实元素 `.os-backdrop`（引擎直接按洞打 mask，保留
  `body::before` 兼容分支）；②新增「祖先层逐层挖洞」（`data-native-cut` + 逐层 mask，
  自动覆盖 `div[vsrc]` 等所有会绘制的中间层）。带真实会话隔离实例真机验证：设置表单/
  网页内容均透过洞正常显示；真实点击分别落入设置视图与网页标签（计数均验证）。

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
- **原生窗口 leader 键抓取（会话焦点交接）：焦点在原生内容时 OS 布局快捷键保持可用**
  （`desktop/main.js` + `desktop/leader-grab.js`（新，含 `leader-grab.test.js`）+
  `desktop/electron-adapter.mjs` + `desktop/remote-preload.js`；平台侧配套在 aic 仓
  `ui/layout/os.html`；设计唯一源 = aic/docs/os_native_windows.md §6）：
  此前点击进原生内容（AI 标签 / 设置 hostView）后壳侧 `wc.focus()` 把键盘焦点整体交给
  内容，平台页收不到 keydown，leader（编排模式 / launcher / 窗口动作）全部失效（v1 起
  已知行为）。现壳对每个原生内容 view 挂 `before-input-event`：leader 集合（页面经
  `native:leader` 同步，默认空 = 不抓取、旧平台页自然降级）精确命中 → preventDefault
  （该键不进内容）+ 经 `native:keys` 转平台页合成 `KeyboardEvent`（复用既有 keymap/编排
  链路）+ 键盘焦点临时交接平台页；会话期间物理键全由平台页原生接收，leader 释放（壳在
  平台页侧 keyUp 判定）→ 焦点自动交还来源内容视图（launcher 等经 `native:focus` 保留
  焦点）。**不做逐键转发的原因**：Chromium 对被处理（handled）的 keyDown 会连带抑制其后
  所有 keyUp/char（`render_widget_host_impl.cc` 的 `suppress_events_until_keydown_`），
  壳侧观测不到 leader 释放、编排退出必然卡死（Electron issue #37336 官方确认 intended）
  —— 焦点交接后释放是平台页上的真实事件，该限制自然绕开。中止清理：应用失焦 / 平台页
  reload 或崩溃 / 平台焦点被用户移走。已知代价：与 leader 集合精确相等的组合（默认 Alt）
  在原生内容中不再可用（macOS Option+←/→ 词跳转 / Option 字符输入等）；leader 可在设置
  改键规避；多修饰键 leader 的前置分量先落内容（集合完成才起交接）。
- 验证：`node --check` 全绿；`leader-grab.test.js` 5 例全绿（normMods / modsOfInput 两端
  归一 / leaderHit 进入判定 / leaderReleased 释放判定 / 载荷）；`vhtml check`（aic 仓
  os.html）OK。
- 生效：桌面端重启后生效（main.js/preload 改动）；平台页随 aic 仓热更新。

### 变更

- **browser screenshot 对齐 §2.2 图片投递标准：超 600KB 端内阶梯压缩后附 image_data**
  （`browser/src/tools/browser/core.js`）：此前仅按 base64 长度硬阈值（1.4M 字符）判断，
  超限直接降级仅 path，与 aic 规范「≤600KB 端内压缩」不符。现与 vcore image.go /
  page_fs.js 同算法（质量 80/60/40 → 0.5 倍逐级缩尺寸，JPEG、白底）在页面环境经
  evalIn 压缩（desktop 主进程/扩展 SW 均无 canvas 的公共交集），结果附 `image_data` +
  `image_compressed` 备注（格式与 cua snapshot --png / fs.read 一致）；压缩失败才降级仅
  path 并在 content 显式提示（不静默丢图）。原图仍整幅落 `/screenshot/`。单测
  （core.test.js）/文档（browser-client.md、background.js help、README）同步。
- **RTC 直连新增 writebin 二进制字节入口（与 readbin 对称，2026-09-12）**：帧协议新增
  直连控制台私有 op `writebin`（不属于 fs 指令集）——载荷 = 文件字节 base64（≤frameInlineLimit
  随 head `text` 内联，超限 `stream:true` + chunk×N + end 重组解码），执行体 =
  `vcore.WriteBin`（新 `libs/vcore/writebin.go`：Resolve→CheckPath→CheckPolicy write 级
  （deny 恒拒）、MkdirAll 0o755、0o644 整文件覆写、256MB 上限），host client 以同信任级
  （granted=9）接线。用途：平台 `$fs.put` 的二进制内容（PNG/zip 等）经此落盘（页面侧此前
  文本化导致二进制损坏，平台侧配套修复）。验证：`rtc_test.go` 新增内联/流式/错误帧四例 +
  `writebin_test.go` 四例；`go test ./libs/vcore ./libs/rtc` 全绿。

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
- **策略错误保型上抛：cloud 内联执行链的审批/拒绝不再被拍平**（`libs/proto/errors.go` +
  `libs/vcore/{result,curl,fileops,json,write}.go`）：执行链在把 VFS 错误归一为 exec/fs
  指令错误（`%s` 拍文案）之前，先经新增的 `proto.StrategyError` 提取 `*proto.ApprovalError` /
  `*proto.DeniedError` 并保型上抛——审批/拒绝语义靠类型判定（StateOf / 信封 / 审批流），
  拍平即降级为普通执行错误。背景：cloud 内联执行没有 host 信封通道（StateOf→NeedApproval
  转换只在 host 侧），GatedFS 写分级兜底依赖 proto 类型判定进入审批流——此前 curl -o /
  json / fs write 等遇 3 级写区报「fs: write requires fs level 3」却不弹审批。修复：
  `execVFSErr` / `fsVFSErr` 接入 curl -o、fs write/edit/cp/mv/rm、json（host 信封路径与
  cloud 内联路径两条语义现各自接全）。验证：`go test ./libs/proto ./libs/vcore` 全绿
  （保型（含 wrapped 链）/ 非策略归一 / curl -o 集成三组新用例）。
- **沙箱放行系统 CA 只读：修复沙箱内 curl error 77**（`libs/fsauth/sysca*.go`（新）+
  `libs/exec_procs/{exec_procs,sandbox,spawn}.go`；文档 `docs/host_sandbox.md` §12.7）：
  deny 通用表 `**/*.pem` 的意图是私钥/凭证，但同样命中系统公共 CA bundle——沙箱内 curl
  等因无法加载证书 https 全断（stat/读被拒，error 77；网络层本身正常，`-k` 可绕）。
  新增**只读**放行通道：`fsauth.SystemCAReadPatterns()`（darwin：`/etc/ssl/cert.pem`、
  `/etc/ssl/certs/**`、Homebrew `openssl@*`/`ca-certificates`（arm64/intel 双前缀）；
  linux：发行版 CA 目录；windows nil；与 deny 同口径 canonical+字面双形态）→
  `confineSpec.readAllow` → darwin seatbelt 在 deny/override 之后追加 `(allow file-read*)`，
  恒不输出 `file-write*`（写系统 CA = 自定义信任根注入）；linux bwrap 并入覆盖判定
  （当前 no-op，对齐防护）。验证：seatbelt 形态断言（allow 在 deny 后 / 写级无 write
  allow / 零值无输出）+ 平台清单用例 + 沙箱内 `curl https://…`（默认 CA）直 200。
- **壳 provider 注册竞态：启动窗口期注册丢失导致 caps 缺 browser**（`libs/host/runtime.go` +
  `libs/host/register.go`）：register 请求可能先于会话就绪到达（`rtClient` 尚未赋值），
  彼时只更新进程级 provider 注册表、未并入已构建的命令表，Connect 尾部发布的 caps
  因此缺少 browser（平台判 exec browser 未声明而拒绝）。修复：Start 在 `rtClient`
  赋值后对账一次（`syncProviders`），有变化补发 caps。实测依据：09-12 17:39 日志
  `provider/register`(200) → `connected to NATS` → `caps published (14 commands)`。
- **后端子进程孤儿看门狗**（`cli/main.go`）：desktop 形态（Electron 壳 spawn）下父进程
  被强杀/崩溃时不会走 will-quit 清理，子进程滞留为孤儿；新增父进程看门狗（轮询 ppid，
  消失即自行退出）。仅 desktop 形态启用，cli 常驻用法不受影响（Windows 无此语义不触发）。
- **main.js will-quit 回收注册提前**（`desktop/main.js`）：杀后端子进程的 will-quit
  原先注册在 start() 末尾，spawn 超时/启动中途失败等提前退出路径漏 kill 留下孤儿；
  改为启动流程最前注册（幂等）。
- **browser 扩展 page_fs.js 与平台侧恢复逐字节同步**（`browser/src/sdk/page_fs.js` +
  `browser/src/sdk/file_search.js`（新）+ `browser/options/files.js`）：两副本自 09-06
  起漂移——平台侧 search() 已重构为 file_search basename glob 契约（glob/limit/depth、
  仅匹配文件名、大小写敏感），扩展侧仍是旧 walk+子串签名；本次恢复 `cmp` 逐字节一致，
  扩展 options 搜索改用结构化签名 `search("/", {glob, limit})`（查询转 `*q*` glob，
  特殊字符映射 '?' 与 shortcut_fs 同款）。`node --test`（扩展）127 例全绿。

> 排查背景：09-10/11 桌面端 UI 测试期由 exec 沙箱内反复启动测试实例（每轮 spawn 一个
> 后端），测试替换时漏回收；且沙箱下读不了 config.yaml（无 key 不连平台）、写不了
> 日志（lumberjack 轮转 rename 被拒）——累计残留 5 个测试实例后端（完全隐形、占端口
> 空转），09-12 已逐一排查清除。

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
