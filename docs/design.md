# AIC Pod 设计文档

## 概述

AIC Pod 是 AIC 平台客户端程序仓库。客户端以独立进程/插件形式运行在各类终端设备上，通过 NATS over WebSocket 连入 AIC 服务端，将设备上的执行能力（命令执行、文件操作、浏览器控制等）注册为 LLM 可调用的工具。

每个客户端是**能力代理**——它不决策做什么，只忠实地在本地执行服务端发来的指令并返回结果，同时通过验签确保指令来源可信。

## 目录结构

```
aic-pod/
├── go.mod                # module github.com/veypi/aic-pod
├── Makefile              # 构建/发版（build/all/build-browser/docker-build/release）
├── Dockerfile            # 容器镜像（ENTRYPOINT ["aic","run"]）
│
├── sdk/                  # Go SDK：协议 + host 运行时 + 指令引擎 + 子进程托管
│   ├── proto/            # 协议层：subject 拓扑、请求/响应信封、HMAC 签名（HKDF 三密钥派生）、
│   │                     #   caps v2、客户端版本门禁、nonce 防重放（固定向量测试锁定）
│   ├── host/             # host agent 运行时：NATS 连接/重连/认证失败处理、caps 发布、心跳、
│   │                     #   请求分发（验签→deadline→防重放→纵深检查）、统一命令声明表、
│   │                     #   配置模型（cli/desktop 共享 config.yaml）、Runner（会话生命周期）、
│   │                     #   LocalAPI（本地管理 API，vigo 框架）、RingBuffer（日志缓冲）
│   ├── vcore/            # 虚拟指令引擎：统一命令声明表与分级表（meta/levels 同包维护）、
│   │                     #   ls/rg/tree/curl/rm/mkdir/cp/mv/git/browser/bg_*/commands 实现、
│   │                     #   OS VFS 适配、argv 双层解析
│   └── exec_procs/       # 子进程统一托管：stdout+stderr 合并落盘、请求超时自动后台化、
│                         #   bg_list/bg_wait/bg_kill、进程组终止
│
├── cli/                  # 命令行入口：aic（vigo/flags 解析，主命令运行，无子指令）
├── desktop/              # Wails v3 桌面壳（Go）：平台窗口 + 进程内 host 会话（Runner）
│                         #   + 首页连接（local_code 通道复用 sdk/host LocalAPI）
├── browser/              # Chrome MV3 扩展（原生 JS ESM）：Service Worker 运行时、
│                         #   browser 工具（agent-browser 签名对齐）、vcmd/page_fs（与 aic 前端双端同步）
├── docs/                 # design.md（本文）、browser-client.md、host_sandbox.md（待实施规划）
└── dist/                 # 构建产出（make 生成）
```

### 设计原则

- **一个子目录一种客户端**：目录名即客户端身份，不做交叉依赖
- **SDK 按语言分层**：`sdk/`（Go）、浏览器端 `browser/src/sdk/`（JS）各自独立，客户端只引入同语言 SDK
- **入口最小化**：客户端目录仅包含入口代码（main.go 等），核心逻辑全部在 SDK
- **外部可扩展**：任何人引用 Go SDK 即可编写自定义客户端，无需修改本仓库

## 客户端类型

### cli — 命令行 host agent

| 维度 | 说明 |
|------|------|
| **语言** | Go |
| **目标平台** | Windows / macOS / Linux |
| **权限模型** | 最高（全盘文件、完整 shell 逃生舱） |
| **能力** | exec（统一命令声明表：vcore 虚拟指令 + 启动探测的 shell/git/browser）、fs（read/write/edit） |
| **典型场景** | 开发服务器、个人 PC、CI Runner、Docker 容器 |
| **体积** | ~10 MB 单二进制 |
| **参数** | `-host`（平台地址，NATS 端点由此推断）+ `-key`；配置链 flag > env（HOST/KEY/WORK_DIR/EXEC_TIMEOUT）> config.yaml > 默认 |

### desktop — Wails v3 桌面壳

| 维度 | 说明 |
|------|------|
| **语言** | Go |
| **目标平台** | Windows / macOS / Linux |
| **形态** | 主窗口加载平台页面（-host，URL 携带 ?local_code={port}.{code}），进程内托管 host 会话（sdk/host Runner） |
| **能力** | 窗口、host 生命周期（启动/停止/日志）、host + 凭据配置（平台 /hosts 页经 sdk/host LocalAPI） |
| **典型场景** | 个人 PC 桌面端，页面直连平台、本机能力经进程内 host 注册 |

### browser — Chrome 扩展

| 维度 | 说明 |
|------|------|
| **语言** | 原生 JS（ESM，MV3 Service Worker） |
| **目标平台** | Chrome（Firefox 未做） |
| **密钥派生** | Web Crypto API（SubtleCrypto，与 Go 端 HKDF 同语义） |
| **能力** | browser 指令（DOM 操作、截图、标签页管理）+ vcmd 核心虚拟指令（ls/rg/tree/rm/curl，操作扩展 PageFS） |
| **限制** | 浏览器沙箱：无 shell、无系统文件系统 |

### embedded / mobile — 未来规划（未实现）

| 形态 | 说明 |
|------|------|
| **embedded** | Go（可能 tinygo）：受限白名单命令 + 限定目录，目标树莓派/IoT/边缘节点（< 5MB） |
| **mobile** | Dart/Flutter：iOS/Android，系统沙箱 + 用户授权（拍照/定位/通知/传感器） |

## 指令模型（指令集 v2.5）

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

- **恒声明**：核心 8 虚拟指令（ls/rg/tree/curl/rm/mkdir/cp/mv）+ commands + bg_list/bg_wait/bg_kill（vcore 元数据同源）
- **启动探测**（exec.LookPath）：shell（bash/zsh/sh/fish/powershell/pwsh/cmd）→ level 3 逃生舱；git → level 1（本地凭证天然可用）；agent-browser CLI → browser 指令
- 分级与动态提升（git push/checkout/reset、browser upload、rm -r 非空目录 → Danger）见 `sdk/vcore/levels.go`

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

### Go SDK（`sdk/`）

| 子包 | 职责 |
|------|------|
| `sdk/proto` | 协议层唯一权威：subject 构造/解析（连接级）、ToolRequest/ToolResponse 信封、HKDF 三密钥派生、连接 token 与请求签名（canonical 输入 + HMAC-SHA256）、caps v2、版本门禁、nonce 防重放。固定向量测试锁定双端一致。 |
| `sdk/host` | host agent 运行时：NATS 连接（TokenHandler 动态签发连接 token）/重连（republish caps）/认证失败处理、caps v2 发布、20s 心跳、请求分发（验签→deadline→nonce 去重→granted_level 纵深检查）、统一命令声明表构建、fs/exec/browser/bg_* 路由、配置模型（Config/解析链/原子持久化）、Runner（host 会话生命周期，cli/desktop 共用）、LocalAPI（本地管理 API：127.0.0.1 随机端口 + local_code 通道，vigo 框架实现）、RingBuffer（日志环形缓冲，挂入 vigo/logv 供 get_log 读取）。 |
| `sdk/vcore` | 虚拟指令引擎：命令声明表与分级表同包维护（meta.go/levels.go）、ls/rg/tree/curl/rm/mkdir/cp/mv/git 等内存实现、OS VFS 适配接口（OSVFS/memvfs）、argv 双层解析、图片尺寸/压缩。 |
| `sdk/exec_procs` | 子进程统一托管：stdout+stderr 合并落盘日志、请求 deadline 超时自动后台化（进程继续运行）、输出前 1000 行截断 + truncated + path、bg_list/bg_wait/bg_kill、进程组 SIGTERM→5s SIGKILL。 |

**客户端只需做的：**

```go
cfg, _ := host.Load()                       // 配置文件 + env 覆盖
opts, _ := cfg.Options("cli", "v1.2.3", nil)
c, _ := host.Connect(opts)                  // 连接并阻塞
```

### TypeScript SDK / Dart SDK — 未来

浏览器端逻辑现位于 `browser/src/sdk/`（JS ESM：client/proto/auth/crypto/vcmd/page_fs/history），与 Go SDK 语义对齐（subject/信封/签名同源，vcmd.js 与 aic 前端逐字节同步）。独立 TS SDK 与 Dart SDK（Flutter mobile）为未来规划。

## 配置体系

CLI 与 Desktop 共享同一份配置文件：`os.UserConfigDir()/aic/config.yaml`（0600，原子写）。

- 解析由 vigo/flags 承担（`AutoRegister` 自动注册 flag + env，只需配置结构体）：
  **显式 flag > 环境变量 > 配置文件（LoadConfig 填充默认值）> 结构体默认**
- flag：`-host` / `-key` / `-work_dir` / `-exec_timeout`（json tag 即 flag 名）
- env：`HOST` / `KEY` / `WORK_DIR` / `EXEC_TIMEOUT`（字段名大写，无前缀）
- 配置键：`host`（平台地址，默认 https://ivec.ai）、`key`（绑定凭证，必填）、`work_dir`（exec 缺省工作区）、`exec_timeout`（后台超时，默认 30m）
- NATS 端点完全由 host 推断（ResolveNATSURL）：https→wss / http→ws，路径前缀保留并拼接 /aic/api/nc
- 本地管理 API（LocalAPI，sdk/host）：cli run 与 desktop 启动时在 127.0.0.1 随机端口监听，
  打印带 local_code 的引导链接（`{host}/hosts?local_code={port}.{code}`），浏览器访问即绑定/管理本机

## 协议

所有客户端遵循同一套 AIC Env 协议（指令集 v2.5）。协议唯一权威：

- `sdk/proto`（subject/信封/签名，含固定向量测试）
- aic 仓库 `docs/instruction_sets_v2.md` §6（host 协议规范：连接认证、caps v2、工具请求验签/防重放/纵深检查、错误模型）

核心要点：

- NATS over WebSocket（`/aic/api/nc`），连接 token 认证（HMAC-SHA256，K_connect）
- HKDF 派生 K_connect / K_server / K_tool 三把用途隔离密钥
- 连接级 subject：`u.{uid}.h.{host_id}.{cred_ver}.caps|presence`（生命周期）、`u.{uid}.h.{host}.{tool}.req`（工具请求，session_id 由信封携带）
- 即时发布 CAPS → 定时心跳（20s）→ 单订阅 inbox（`u.{uid}.h.host_{host_id}.>`）→ 验签执行 → req-reply 回复

## 外部扩展

外部成员编写自定义客户端只需：

1. 引入 Go SDK（`github.com/veypi/aic-pod/sdk/host` + `sdk/proto`）
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
| **Phase 1** | `sdk/` + `cli`/`desktop` — host agent 运行时、统一命令声明表（指令集 v2.5）、配置体系 | 完成 |
| **Phase 2** | `embedded` — 适配 tinygo、命令白名单、限定目录 | 规划（未开始） |
| **Phase 3** | `browser` — Chrome MV3 扩展（Web Crypto、browser 指令、vcmd/page_fs 双端同步） | 完成 |
| **Phase 4** | `mobile` — Dart/Flutter App | 规划（未开始） |
