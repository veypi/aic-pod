# AIC Pod 设计文档

设备能力以 [三协议实现说明](hosts-tools.md) 和 [统一结构图](hosts-protocols-proposal.md) 为准。下文其他模块说明保留；旧设备传输入口已移除。

## 概述

AIC Pod 是 AIC 平台客户端程序仓库。客户端以独立进程形式运行在各类终端设备上，通过 NATS over WebSocket 连入 AIC 服务端，将设备上的执行能力（命令执行、文件操作、浏览器控制、原生 GUI 自动化、ssh/scp 转发等）注册为 LLM 可调用的工具。本机能力受三域授权（fs/net/ssh）与进程沙箱两道闸控制（详见 [host_sandbox.md](host_sandbox.md)）。

每个客户端是**能力代理**——它不决策做什么，只忠实地在本地执行服务端发来的指令并返回结果，同时通过验签确保指令来源可信。

## 目录结构

```
aic-pod/
├── go.mod                # module github.com/veypi/aic-pod
├── Makefile              # 构建/发版（build/cli-all/desktop-all/docker-build/release）
├── Dockerfile            # 容器镜像（ENTRYPOINT ["aic","run"]）
│
├── init.go               # 根包 pod：本地服务装配（Router + Start/Stop）
├── cfg/                  # 配置中心：Options + Global（含 Version/DeviceType）、config.yaml 0600 原子写
├── api/                  # 本地管理 HTTP 层（包级 Router：127.0.0.1 随机端口 + local_code 通道，
│                         #   vigo 声明式 handler + 统一 JSON 响应；ui/settings.html 静态资源）
├── libs/                 # 客户端核心：协议 + host 运行时 + 指令引擎 + 子进程托管
│   ├── proto/            # 协议层：subject 拓扑、请求/响应信封、HMAC 签名（HKDF 三密钥派生）、
│   │                     #   caps v2、客户端版本门禁、nonce 防重放（固定向量测试锁定）
│   ├── host/             # host agent 运行时：NATS 连接/重连/认证失败处理、caps 发布、心跳、
│   │                     #   请求分发（验签→deadline→防重放→纵深检查）、统一命令声明表、
│   │                     #   fs/exec/bg_* 路由、壳 provider 注册、cua 桥接（cua.go/cua_run.go）、
│   │                     #   配置模型（cli/desktop 共享 config.yaml）、Runner（会话生命周期）
│   ├── vcore/            # 虚拟指令引擎：命令声明表与分级表（meta/levels 同包维护）、
│   │                     #   curl/json/bg_*/commands + git/browser/cua/ssh/scp 元数据与分级、
│   │                     #   fs 8 action 实现、OS VFS 适配、argv 双层解析
│   ├── fsauth/           # 文件授权：fs 域判定（policy/deny/allow、内置根、会话临时 grant、env 清洗）
│   ├── netauth/          # 网络授权：net 域出站闸（deny/allow、localhost 内建、沙箱网络规则）
│   ├── rtc/              # WebRTC 直连应答（2026-09-10）：单 UDP mux + mDNS 解析 + DataChannel
│   │                     #   鉴权帧 + fs 帧协议 + readbin/writebin 二进制字节出入口（预览/下载/二进制写入，见 design.md「协议」节）
│   └── exec_procs/       # 子进程统一托管：沙箱包装（seatbelt/bwrap/受限令牌）+ 日志落盘 +
│                         #   请求超时自动后台化 + bg_list/bg_wait/bg_kill + 进程组终止
│
├── cli/                  # 命令行入口：aic（vigo/flags 解析，主命令运行，无子指令）
├── desktop/              # Electron 壳（纯远程）：窗口直接加载平台页 + session.setPreloads 注入
│                         #   remote-preload（host 白名单 → window.aicDesktop：api 转发/窗口控制）
│                         #   + 端口握手（AIC_PORT_FILE）+ 窗口控制 IPC（preload contextBridge）
│                         #   browser-path.cjs 仅注入 Chrome 默认路径；Go libs/browser 独立执行
│                         #   内置 cua-driver 发行物（cua.json + scripts/sync-cua.mjs → resources/cua）
├── protocol/ui/          # ui/1 命令 schema、Go 解析/结果、跨语言验收向量
├── docs/                 # design.md（本文）、host_sandbox.md（沙箱与三域授权）、ui-protocol.md
└── dist/                 # 构建产出（make 生成）
```

### 设计原则

- **一个子目录一种客户端**：目录名即客户端身份，不做交叉依赖
- **交互协议同源**：Go 和 JS 读取 `protocol/ui/schema.json`；desktop/browser 直接驱动 CDP，原生 cua 由 Go 适配 cua-driver
- **入口最小化**：客户端目录仅包含入口代码（main.go 等），核心逻辑全部在 libs/api
- **外部可扩展**：任何人引用 Go libs 即可编写自定义客户端，无需修改本仓库

## 客户端类型

### cli — 命令行 host agent

| 维度 | 说明 |
|------|------|
| **语言** | Go |
| **目标平台** | Windows / macOS / Linux |
| **权限模型** | 高（沙箱 + 三域授权门控；shell 为 level 3 逃生舱） |
| **能力** | exec（统一命令声明表：核心虚拟指令 + 启动探测的 shell/git/ssh/scp）、fs（8 action）、ssh/scp |
| **典型场景** | 开发服务器、个人 PC、CI Runner、Docker 容器 |
| **体积** | ~10 MB 单二进制 |
| **参数** | `-host`（平台地址，NATS 端点由此推断）+ `-key`；配置链 flag > env（HOST/KEY/WORK_DIR/EXEC_TIMEOUT）> config.yaml > 默认 |

### desktop — Electron 纯远程壳 + Go 后端子进程

| 维度 | 说明 |
|------|------|
| **语言** | Node（主进程）+ Go（后端二进制） |
| **目标平台** | Windows / macOS / Linux |
| **形态** | 启动：loading → spawn 后端（AIC_PORT_FILE 握手）→ 探测 {host}/root.html → 主窗口加载平台页（Chromium）；session.setPreloads 注入 remote-preload（白名单 = 配置 host + 默认域名与旧域名 ivec.ai），平台页经 window.aicDesktop 直调本地 API（端口/code 不出主进程，IPC handler 校验 senderFrame host） |
| **能力** | 与 cli 相同（exec/fs/ssh/scp，沙箱 + 三域授权）；另有 `hosts_tools/1` 的 `browser`（独立 Chrome）与 `cua`（cua-driver MCP 桥接，原生 GUI 自动化，内置发行物随包分发） |
| **本地页面** | 仅 /settings 配置页（独立系统边框配置窗口：托盘「本地配置」直开，平台不可达首配时自动打开）；设置保存后探测并跳 {host}/hosts |
| **桌宠** | 透明小窗加载 {host}/pet（平台页，双击恢复 + IPC 拖动） |
| **典型场景** | 个人 PC 桌面端，页面直连平台、本机能力经 host 注册 |

### embedded / mobile — 未来规划（未实现）

| 形态 | 说明 |
|------|------|
| **embedded** | Go（可能 tinygo）：受限白名单命令 + 限定目录，目标树莓派/IoT/边缘节点（< 5MB） |
| **mobile** | Dart/Flutter：iOS/Android，系统沙箱 + 用户授权（拍照/定位/通知/传感器） |

## 安全模型

两道闸（判定唯一权威见 [host_sandbox.md](host_sandbox.md)，实现见 `libs/fsauth`、`libs/netauth`、`libs/exec_procs`）：

- **三域授权**（fs / net / ssh × policy / deny / allow），统一判定式：
  `deny 命中 → 拒，除非存在更具体的 allow（具体度优先，同精度 deny 胜）；
  policy=open → 未命中 deny 一律放；policy=deny → 仅 allow 放行`。
  fs 域读默认开、写受 policy + 内置可写根 + `fs_allow` + 临时 grant 控制（显式 allow 可覆盖 deny）；
  net 域管沙箱内子进程出站（内建 localhost:*）；ssh 域是 ssh/scp 一级工具的目标闸。
  `set_config` 与 `grant <域> <目标> [--permanent]` 动态生效（已启动进程不回溯）。
- **进程沙箱**：exec 调用默认进沙箱（darwin seatbelt / linux bubblewrap / windows
  受限令牌 + 能力 SID ACL），按授予等级选 profile（1=read-only，2/3/4/9=workspace-write），
  叠加 env 敏感变量清洗、资源限制与网络闸；无可用后端 **fail-closed**。
  免沙箱唯一通道 = 请求级 `nosandbox` + 单独人工审批（Critical(4)）——审批通过（9）
  本身不豁免沙箱。

cua / browser / ssh / scp 属宿主体外或独立通道能力，不进 exec 沙箱，各自受目标闸与等级表控制。

## 指令模型（指令集 v2.6）

协议与指令语义以 aic 仓库 `docs/instruction_sets_v2.md` 为权威，本仓库实现并维护同源表（meta/levels）。

### 参数风格

所有工具统一采用 **Linux 命令风格**：`action + argv`。

```json
{ "action": "read", "argv": ["/etc/hosts", "--offset", "10", "--limit", "50"] }
```

**argv 解析规则：**

- 非 `--` 开头的为位置参数
- `--key value` 为键值对（下一个非 `--` 开头的为 value）
- `--key` 单独出现为 bool 标记
- 顺序自由，`--flags` 可出现在位置参数前后
- 未声明 flag 一律拒绝（受限反馈），单横线 flag 支持组合展开（-la）

### 统一命令声明表（§5.1）

所有 exec 命令统一声明 `{name, desc, help, level}`，未声明命令一律拒绝（不存在「未知命令透传」）：

- **恒声明**：核心虚拟指令（`curl`/`json` + `commands`/`bg_list`/`bg_wait`/`bg_kill`，vcore 元数据同源）
- **fs 指令集**（独立工具，8 action）：`read`/`write`/`edit`/`ls`/`rg`/`cp`/`mv`/`rm`
- **启动探测**（exec.LookPath）：shell（bash/zsh/sh/fish/powershell/pwsh/cmd）→ level 3 逃生舱；git → level 1（本地凭证天然可用）；ssh/scp → 独立目标闸（ssh 域 Policy）；cua（cua-driver 二进制）→ 本机 GUI 自动化（§5.10）
- **工具注册**：browser/cua 经 hosts_tool 一次声明，由 hosts_rtc/1 与 hosts_nats/1 调用，详见 [当前实现](hosts-tools.md)
- 分级与动态提升（git push/checkout/reset、browser 子命令、cua `--delivery foreground`/`activate`、rm -r 非空目录 → Danger）见 `libs/vcore/levels.go`

### fs — 文件操作

| 字段 | 值 |
|------|-----|
| `name` | `fs` |
| `required_level` | read=1 (Read)，write/edit=2 (Write) |

| action | argv | 说明 |
|--------|------|------|
| `read` | `<path> [--offset N] [--limit N]` | 读取文件（host 端可返回 image_data，§2.2 图片投递收敛） |
| `write` | `<path> --content <string>` | 写入文件（覆盖） |
| `edit` | `<path> --old <string> --new <string> [--replace-all]` | 替换内容 |
| `ls` | `<path> [--depth N]` | 列目录（递归树） |
| `rg` | `<pattern> <path> [--glob G] [--context N] [--limit N]` | 内容搜索 |
| `cp` / `mv` | `<src> <dst>` | 复制 / 移动（目录递归） |
| `rm` | `<path> [--recursive]` | 删除（删非空目录提级 Danger(3)） |

物理 host 的路径为本地绝对路径；cloud/page 走 UFS/PageFS（见 instruction_sets_v2.md §2.1.1）。

### 自定义命令

自定义命令注册进统一命令声明表，遵循同样的 `action + argv` 风格：

1. **action 命名**：用动词或短名词，一个工具可以有多个 action
2. **位置参数在前**：核心对象（路径、文件名等）不放 flag 里
3. **可选参数用 flag**：用 `--key value` 或 `--bool-flag` 风格
4. **保持正交**：不同 action 的 flag 含义一致。例如 `--offset` / `--limit` 在所有读取型 action 中语义相同

注册声明示例（Go）：

```go
c.RegisterCommand(proto.CommandDecl{
    Name: "camera", Desc: "Capture photos and video. Actions: capture, stream, info.",
    Help: "camera capture [--width 1920]...\n  ...", RequiredLevel: proto.LevelRead,
})
```

## SDK 设计

### Go 核心库（`libs/`）与本地 API（`api/`）

| 子包 | 职责 |
|------|------|
| `libs/proto` | 协议层唯一权威：subject 构造/解析（连接级）、ToolRequest/ToolResponse 信封、HKDF 三密钥派生、连接 token 与请求签名（canonical 输入 + HMAC-SHA256）、caps v2、版本门禁、nonce 防重放。固定向量测试锁定双端一致。 |
| `libs/host` | host agent 运行时：NATS 连接（TokenHandler 动态签发连接 token）/重连（republish caps）/认证失败处理、caps v2 发布、20s 心跳、请求分发（验签→deadline→nonce 去重→granted_level 纵深检查）、统一命令声明表构建、fs/exec/browser/bg_* 路由、配置模型（Config/解析链/原子持久化）、Runner（host 会话生命周期，cli/desktop 共用）。 |
| `libs/vcore` | 虚拟指令引擎：命令声明表与分级表同包维护（meta.go/levels.go）、curl/json 等虚拟指令与 fs 8 action 实现、git/browser/cua/ssh/scp 元数据与动态分级、OS VFS 适配接口（OSVFS/memvfs）、argv 双层解析、图片尺寸/压缩。 |
| `libs/fsauth` | 文件授权：fs 域判定（policy/deny/allow 匹配、内置根、会话级临时 grant、沙箱 deny 模式展开、env 敏感变量清洗）。 |
| `libs/rtc` | RTC 直连应答（2026-09-10）：pion/webrtc 集成——单 UDP mux、mDNS QueryOnly、信令处理（offer/answer/trickle）、DataChannel 鉴权帧、fs 帧协议服务（chunk 流式 + backpressure）+ readbin/writebin 二进制字节出入口（vcore.ReadBin/WriteBin，预览/下载与二进制写入用）。 |
| `libs/netauth` | 网络授权：net 域出站判定（deny 恒优先于 allow、localhost 内建、具体度排序、沙箱网络规则生成）。 |
| `libs/exec_procs` | 子进程统一托管 + 沙箱：seatbelt/bubblewrap/受限令牌包装（按等级选 profile、env 清洗、资源限制、网络闸，无可用后端 fail-closed）、stdout+stderr 合并落盘、请求 deadline 超时自动后台化（进程继续运行）、输出前 1000 行截断 + truncated + path、bg_list/bg_wait/bg_kill、进程组 SIGTERM→5s SIGKILL。 |
| `api` | 本地管理 API：包级 Router（security 中间件 + common.JsonResponse/JsonErrorResponse 统一响应），127.0.0.1 随机端口 + local_code 通道（端点 ping/get_config/set_config/bind/unbind/get_status/get_log/start/stop + /settings 设置页静态资源）；host 会话生命周期自持（libs/host Runner，Init(deviceType, version) 创建，Start 时自动连接已绑定设备）；外链由壳页面处理（Electron 系统浏览器 IPC / 浏览器壳新标签，平台页 local_handler 拦截后 postMessage 转交）；有效配置读写 cfg.Global，get_log 读日志文件尾部。 |
| `cfg` | 配置中心：Options 结构体（flag/env/文件/default 四级解析，port/code 进程级隐私字段）+ Global 全局有效配置 + Load/LoadFile/Save（config.yaml 0600 原子写）+ LogPath/LogWriter（aic.log console 格式滚动写入，cli console+文件双写、desktop 仅文件）。 |

**客户端只需做的：**

```go
cfg, _ := host.Load()                       // 配置文件 + env 覆盖
opts, _ := cfg.Options("cli", "v1.2.3", nil)
c, _ := host.Connect(opts)                  // 连接并阻塞
```

### TypeScript SDK / Dart SDK — 未来

desktop 的 UI 自动化位于 `desktop/browser/`；跨语言命令协议位于 `protocol/ui/`。Chrome 扩展已移除，AIC page 的文件能力继续由平台前端独立维护。

## 配置体系

CLI 与 Desktop 共享同一份配置文件：`os.UserConfigDir()/aic/config.yaml`（0600，原子写）。

- 解析由 vigo/flags 承担（`AutoRegister` 自动注册 flag + env，只需配置结构体）：
  **显式 flag > 环境变量 > 配置文件（LoadConfig 填充默认值）> 结构体默认**
- flag：`-host` / `-key` / `-work_dir` / `-exec_timeout` / `-home_path`（json tag 即 flag 名）
- env：`HOST` / `KEY` / `WORK_DIR` / `EXEC_TIMEOUT` / `HOME_PATH`（字段名大写，无前缀）
- 配置键：`host`（平台地址，默认 https://ivec-ai.com）、`key`（绑定凭证，必填）、`work_dir`（exec 缺省工作区）、`exec_timeout`（后台超时，默认 30m）、`home_path`（desktop 默认打开地址，host 后路径，默认 `/`，必须 `/` 开头；清空恢复 `/`）、`code`（本地 API 校验码，空 = 进程级随机）、`rtc`（RTC 直连应答开关，默认 true；关闭则 caps 不上报 mgmt、不应答 rtc.in 信令）
- 三域授权键（vigo/flags 自动注册 flag/env，env 名 = json tag 大写）：
  `fs_policy`/`fs_deny`/`fs_allow`、`net_policy`/`net_deny`/`net_allow`、`ssh_policy`/`ssh_deny`/`ssh_allow`；
  隐藏项 `no_sandbox`（全局跳过 exec 沙箱，仅配置文件/flag/env 可改，本地管理 API 不暴露）
- 发版版本位：只改 `cfg/config.go` 的 `Version`（带 `v` 前缀）；
  `desktop/package.json` 由 `make desktop-version` 从 git describe 自动同步
- NATS 端点完全由 host 推断（ResolveNATSURL）：https→wss / http→ws，路径前缀保留并拼接 /api/nc
- 本地管理 API（api 包 Router）：cli 与 desktop 启动时在 127.0.0.1 随机端口监听，
  打印带 local_code 的引导链接（`{host}/hosts?local_code={port}.{code}`），浏览器访问即绑定/管理本机

## 协议

所有客户端遵循同一套 AIC Env 协议（指令集 v2.6）。协议唯一权威：

- `libs/proto`（subject/信封/签名，含固定向量测试）
- aic 仓库 `docs/instruction_sets_v2.md` §6（host 协议规范：连接认证、caps v2、工具请求验签/防重放/纵深检查、错误模型）

核心要点：

- NATS over WebSocket（`/api/nc`），连接 token 认证（HMAC-SHA256，K_connect）
- HKDF 派生 K_connect / K_server / K_tool 三把用途隔离密钥
- 连接级 subject：`u.{uid}.h.{host_id}.{cred_ver}.caps|presence`（生命周期）、`u.{uid}.h.{host}.{tool}.req.{sid}`（工具请求，§6.1 v4——sid 段定向，信封 SessionID 一致；run_tool 无会话直发用 manual 占位）
- 即时发布 CAPS → 定时心跳（20s）→ 单订阅 inbox（`u.{uid}.h.host_{host_id}.>`）→ 验签执行 → req-reply 回复

**设备调用**：前端 hosts_rtc/1 与服务器 hosts_nats/1 共用 hosts_tools/1 声明和分发，内建 FS 与 exec.commands 是唯一能力模型。RTC 票据绑定 DTLS，控制使用 hosts-tools，原始 stream 使用 hosts-stream/*。文件代理转换为签名的 fs-only NATS call。业务状态分别由 FS、exec、Browser、CUA 管理。

## 外部扩展

外部成员编写自定义客户端只需：

1. 引入 Go 库（`github.com/veypi/aic-pod/libs/host` + `libs/proto`）
2. 注册自定义命令（`RegisterCommand`，走统一命令声明表）
3. 编写客户端入口（连接参数、设备信息）

```go
// my-cron/main.go
c := host.New(host.Options{Credential: "..."})
c.RegisterCommand(proto.CommandDecl{Name: "cron", Desc: "..."})
c.Connect()
```

无需 fork 或修改本仓库。

## 路线图

| 阶段 | 内容 | 状态 |
|------|------|------|
| **Phase 1** | `libs/` + `api` + `cli`/`desktop` — host agent 运行时、统一命令声明表、配置体系 | 完成 |
| **Phase 2** | 安全模型 — 三域授权（fs/net/ssh）+ exec 进程沙箱（seatbelt/bwrap/受限令牌） | 完成 |
| **Phase 3** | `browser` / `cua` — ui/1 统一协议；desktop CDP 与原生窗口适配 | 已切换；扩展退场 |
| **Phase 4** | `cua` — 本机 GUI 自动化（cua-driver MCP 桥接，内置发行物随包分发） | 完成 |
| **Phase 5** | `embedded` — 适配 tinygo、命令白名单、限定目录 | 规划（未开始） |
| **Phase 6** | `mobile` — Dart/Flutter App | 规划（未开始） |
