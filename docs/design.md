# AIC Pod 设计文档

## 概述

AIC Pod 是 AIC 平台客户端程序仓库。客户端以独立进程/插件形式运行在各类终端设备上，通过 NATS over WebSocket 连入 AIC 服务端，将设备上的执行能力（命令执行、文件操作、系统信息等）注册为 LLM 可调用的工具。

每个客户端是**能力代理**——它不决策做什么，只忠实地在本地执行服务端发来的指令并返回结果，同时通过验签确保指令来源可信。

---

## 目录结构

```
aic-pod/
├── go.mod                # module github.com/veypi/aic-pod
├── go.sum
├── Makefile
│
├── sdk/                  # Go SDK 包，desktop / embedded 等 Go 客户端引用
│   ├── client.go         # NATS 连接/重连/断线处理 + CAPS 发布 + 心跳 + 工具请求分发（验签/防重放/权限/幂等）
│   ├── auth.go           # Token 生成 + HMAC 签名 + 工具请求验签
│   ├── crypto.go         # HKDF 密钥派生 (K_connect / K_server / K_tool)
│   ├── types.go          # 公共类型定义 (Options/Tool/ToolDef/请求响应载荷)
│   ├── argv.go           # action+argv 双层解析 (flagSet/parseActionArgv)
│   ├── exec.go           # exec 工具实现 (后台进程 bg_list/wait/kill)
│   ├── exec_unix.go      # Unix setpgid + 进程组杀死
│   ├── exec_windows.go   # Windows 空实现
│   ├── fs.go             # fs 工具实现 (10 种 action, 含 download/图片处理)
│   ├── hfs.go            # hfs 结构化文件工具 (浏览器直连, 免签)
│   ├── search.go         # 搜索遍历/glob 匹配/mime 检测
│   ├── image.go          # 图片尺寸/压缩 (600KB 对齐服务端)
│   ├── cache.go          # 幂等缓存
│   └── replay.go         # nonce 防重放缓存
│
├── cli/                  # 命令行 host agent 入口 (Windows / macOS / Linux, Go)
│   └── main.go
│
├── desktop/              # Tauri 桌面应用入口 (窗口/托盘/CLI 生命周期管理, Rust)
│   ├── src-tauri/
│   └── src/
│
├── embedded/             # 嵌入式设备客户端入口 (树莓派 / IoT) (未来)
│   └── main.go
│
├── browser/              # 浏览器插件 (Chrome MV3, 已实现; Firefox 未做)
│   ├── manifest.json
│   └── src/
│
├── mobile/               # 手机端 (iOS / Android) (未来)
│   └── lib/
│
├── dist/                 # 构建产出 (make build)
└── docs/
    ├── design.md
    └── nats_client.md
```

### 设计原则

- **一个子目录一种客户端**：目录名即客户端身份，不做交叉依赖
- **SDK 按语言分层**：`sdk/`、`sdk/ts`、`sdk/dart` 各自独立，客户端只引入同语言 SDK
- **入口最小化**：客户端目录仅包含入口代码（main.go 等），核心逻辑全部在 SDK
- **外部可扩展**：任何人引用对应语言 SDK 即可编写自定义客户端，无需修改本仓库

---

## 客户端类型

### cli — 命令行 host agent

| 维度 | 说明 |
|------|------|
| **语言** | Go |
| **目标平台** | Windows / macOS / Linux |
| **权限模型** | 最高（全盘文件、完整 shell） |
| **能力** | exec（任意命令）、fs（全盘文件操作） |
| **典型场景** | 开发服务器、个人 PC、CI Runner、desktop 后台进程 |
| **体积** | ~10 MB 单二进制 |
| **参数** | `-host`（平台地址，默认 https://ivec.ai）+ `-key`；`-url` 可显式指定 NATS 端点（空 = 按 host 推断 https→wss / http→ws，拼接 /aic/api/nc） |

### desktop — Tauri 桌面应用

| 维度 | 说明 |
|------|------|
| **语言** | Rust (Tauri 2) + HTML/JS |
| **目标平台** | Windows / macOS / Linux |
| **形态** | 主窗口加载平台页面（-host，如 https://ivec.ai），后台托管 cli 进程（sidecar） |
| **能力** | 窗口/托盘、cli 生命周期（启动/停止/日志）、host + 凭据配置 |
| **典型场景** | 个人 PC 桌面端，页面直连平台、本机能力经 cli 注册 |

### embedded — 嵌入式设备客户端

| 维度 | 说明 |
|------|------|
| **语言** | Go (可能切 tinygo) |
| **目标平台** | 树莓派、IoT 设备、边缘节点 |
| **权限模型** | 受限（白名单命令、限定目录） |
| **能力** | exec（白名单）、fs（限定目录） |
| **典型场景** | 智能家居控制器、工控设备、边缘网关 |
| **体积** | 尽可能小 (< 5 MB) |

### browser — 浏览器插件

| 维度 | 说明 |
|------|------|
| **语言** | TypeScript |
| **目标平台** | Chrome / Firefox / Edge |
| **运行时** | Service Worker (后台常驻) |
| **密钥派生** | Web Crypto API (SubtleCrypto) |
| **能力** | 网页 DOM 操作、页面截图、标签页管理 |
| **限制** | 浏览器沙箱：无 shell、无文件系统 |

### mobile — 手机端

| 维度 | 说明 |
|------|------|
| **语言** | Dart / Flutter |
| **目标平台** | iOS / Android |
| **权限模型** | 系统沙箱 + 用户授权 |
| **能力** | 拍照、定位、通知、传感器数据、文件读写（沙箱内） |
| **限制** | 前后台切换受系统管控 |

---

## Tool 设计

### 参数风格

所有工具统一采用 **Linux 命令风格**：`action + argv`。

```json
{
  "action": "read",
  "argv": ["/etc/hosts", "--offset", "10", "--limit", "50"]
}
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `action` | string | 操作名，对应子命令 |
| `argv` | []string | 位置参数和 `--flag` 参数混合排列 |

**argv 解析规则：**

- 非 `--` 开头的为位置参数
- `--key value` 为键值对（下一个非 `--` 开头的为 value）
- `--key` 单独出现为 bool 标记
- 顺序自由，`--flags` 可出现在位置参数前后

### 内置工具

#### exec — 命令执行

| 字段 | 值 |
|------|-----|
| `name` | `exec` |
| `required_level` | 2 (Auto) |

`action` 即要执行的程序名，`argv` 为其参数。支持两种模式：

| action | argv | 行为 |
|--------|------|------|
| `bash` | `["-c", "..."]` | 通过 bash 执行脚本 |
| `sh` | `["-c", "..."]` | 通过 sh 执行脚本 |
| `powershell` | `["-Command", "..."]` | 通过 PowerShell 执行 |
| `ls` / `echo` / `git` / … | `[arg1, arg2, ...]` | 直接执行该命令 |
| `python` / `node` / `go` | `[arg1, ...]` | 直接执行该解释器 |

**输出：** stdout+stderr 合并写入系统临时目录下 `aic/{session_id}/{request_id}.log`（Linux `/tmp`，macOS `$TMPDIR`，Windows `%TEMP%`），响应 `content` 最多返回前 1000 行。`attrs.path` 指向完整日志，Agent 可通过 fs read 查看全文。

**实现要点：** `exec.Command(action, argv...)` 直接调用，`action` 可以是 shell 也可以是任意可执行文件。后台超时默认 30m（`EXEC_TIMEOUT` 可调），请求 deadline 到期自动转后台；设置独立进程组（`setpgid`）。

#### fs — 文件操作

| 字段 | 值 |
|------|-----|
| `name` | `fs` |
| `required_level` | 1 (Confirm)，`rm` 单独 2 |

| action | argv | 说明 |
|--------|------|------|
| `ls` | `<path>` | 列出目录内容，目录项末尾加 `/` |
| `read` | `<path> [--offset N] [--limit N]` | 读取文件。offset 默认 0，limit 默认 1000 |
| `write` | `<path> --content <string>` | 写入文件（覆盖） |
| `edit` | `<path> --old <string> --new <string> [--replace-all]` | 替换内容，`--replace-all` 替换全部出现 |
| `rm` | `<path>` | 删除文件或目录 |
| `mkdir` | `<path>` | 创建目录（含父目录） |
| `cp` | `<src> <dest>` | 复制文件 |
| `mv` | `<src> <dest>` | 移动/重命名文件 |
| `search` | `<root> [--glob <pattern>] [--pattern <substr>] [--limit N] [--ignore-case]` | 搜索文件 |
| `download` | `<path> --from <url> [--max-size MB]` | 从 http(s) 下载文件到本地 |

### 自定义工具

自定义工具需遵循同样的 `action + argv` 风格，参考 Linux 命令设计习惯：

**原则：**

1. **action 命名**：用动词或短名词，一个工具可以有多个 action
2. **位置参数在前**：核心对象（路径、文件名等）不放 flag 里
3. **可选参数用 flag**：用 `--key value` 或 `--bool-flag` 风格
4. **保持正交**：不同 action 的 flag 含义一致。例如 `--offset` / `--limit` 在所有读取型 action 中语义相同

**示例：cron 定时任务工具**

| action | argv | 说明 |
|--------|------|------|
| `list` | `[--pattern <substr>]` | 列出定时任务 |
| `add` | `<expr> --cmd <command>` | 添加 cron 表达式 |
| `remove` | `<id>` | 删除指定任务 |

**示例：camera 摄像头工具**

| action | argv | 说明 |
|--------|------|------|
| `capture` | `[--width 1920] [--height 1080] [--format png]` | 拍照 |
| `stream` | `--duration 10s [--fps 30]` | 录制视频片段 |
| `info` | (无位置参数) | 返回摄像头参数 |

**CAPS 声明示例：**

```json
{
  "name": "camera",
  "description": "Capture photos and video. Actions: capture, stream, info.",
  "parameters": {
    "type": "object",
    "properties": {
      "action": { "type": "string", "enum": ["capture", "stream", "info"] },
      "argv": { "type": "array", "items": { "type": "string" } }
    },
    "required": ["action", "argv"]
  },
  "required_level": 1,
  "policy_version": "1"
}
```

---

## SDK 设计

### Go SDK (`sdk/`)

提供完整的 AIC Env 协议实现，cli、embedded 和 desktop（经 sidecar 复用）直接复用。

**模块划分：**

```
client.go     # NATS 连接/重连/断线 + Connect/Close；内部含 PublishCaps、20s 心跳、工具请求分发
auth.go       # Token 生成 (canonical + HMAC-SHA256) + 工具请求验签
crypto.go     # HKDF 密钥派生 (K_connect, K_server, K_tool)
types.go      # Options/Tool/ToolDef/请求响应载荷类型定义
argv.go       # action+argv 双层解析 (flagSet/parseActionArgv)
exec.go       # 命令执行 (setpgid, 默认 30m 超时, exit code) + bg_list/wait/kill
fs.go         # 文件操作 (10 种 action, 含 download/图片压缩)
hfs.go        # hfs 结构化文件工具 (浏览器直连, 免签, depth 嵌套)
search.go     # 搜索遍历/glob 匹配/mime 检测/文本判定
image.go      # 图片尺寸/压缩 (600KB 对齐服务端投递标准)
cache.go      # 幂等缓存
replay.go     # nonce 防重放缓存
```

**客户端只需做的：**

```go
// desktop/main.go 示例
client := sdk.New(sdk.Options{
    Credential: os.Getenv("ENV_CRED"),
    NATSURL:    os.Getenv("NATS_URL"),
    WorkDir:    "/workspace",
})
client.RegisterTool(sdk.Tool{Name: "exec", ...})
client.Connect()
```

### TypeScript SDK (`sdk/ts`) — 未来

浏览器和 VS Code 插件的共享层。核心差异在于使用 `SubtleCrypto` 替代 Go 的 `crypto/hmac` + `golang.org/x/crypto/hkdf`。

### Dart SDK (`sdk/dart`) — 未来

Flutter 客户端使用，通过 Method Channel 桥接原生能力（相机、定位等）。

---

## 协议

所有客户端遵循同一套 AIC Env 协议，详见 [nats_client.md](./nats_client.md)。

核心要点：
- NATS over WebSocket，Token 认证
- HKDF 派生 K_connect（连接签名）和 K_tool（请求验签）
- 即时发布 CAPS → 定时心跳 → 订阅 tool.*.req → 验签执行 → 回复
- 同一凭据单连接互斥

---

## 外部扩展

外部成员编写自定义客户端只需：

1. 引入对应语言 SDK
2. 实现 `tools` 接口（注册自定义工具）
3. 编写客户端入口（连接参数、设备信息）

**示例：自定义定时任务客户端**

```go
// my-cron/main.go
c := sdk.New(sdk.Options{Credential: "..."})
c.RegisterTool(sdk.Tool{
    Name:    "cron",
    Handler: handleCronRequest,
})
c.Connect()
```

无需 fork 或修改本仓库，`import "aic-pod/sdk/go"` 即可。

---

## 路线图

| 阶段 | 内容 |
|------|------|
| **Phase 1** | `sdk/` + `desktop` — 从 aic 仓库迁移 demo，编译为独立二进制 |
| **Phase 2** | `embedded` — 适配 tinygo，减小体积，添加命令白名单 |
| **Phase 3** | ~~`sdk/ts` + `browser`~~ — Chrome 插件已实现（Web Crypto API，MV3） |
| **Phase 4** | `sdk/dart` + `mobile` — Flutter App |
