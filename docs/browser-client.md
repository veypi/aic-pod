# Browser Client 设计

## 架构

```
┌───────────────────────────────────────────────────────┐
│  插件设置页 (Settings UI)                              │
│  background / incognito / viewport / timeout / ...    │
│  用户配置，写入 chrome.storage.local                   │
└────────────────────┬──────────────────────────────────┘
                     │ 读取配置
┌────────────────────▼──────────────────────────────────┐
│  Service Worker (后台常驻)                             │
│  - NATS over WebSocket 连接                           │
│  - caps v2 发布 (fs/exec 能力声明，§6.3)              │
│  - 连接级 inbox 订阅 u.{uid}.h.host_{host_id}.>        │
│    → 验签/防重放/纵深检查 → exec 指令分发               │
│  - browser core（tools/browser/core.js，平台无关）      │
│    + chrome-adapter.js（chrome.tabs/debugger/...）     │
└───────────────────────────────────────────────────────┘
```

> **协议（指令集 v2.6）：** 本扩展的 exec 命令表 = 恒声明 `commands`（能力发现）+ 注册命令
> `browser`（`exec browser <subcommand> [args...]`）；fs 能力 = 8 action
> （read/write/edit/ls/rg/cp/mv/rm，PageFS+fsops 后端，与 page 端逐字节同源）。
> 全局参数在插件设置页配置，不出现在工具调用中。

> **browser 为平台自有指令集**（2026-09 起）：实现 = 平台无关 core + 宿主适配器，
> desktop 端（Electron CDP）共享同一份 core（同步进 `desktop/vendor/`）。
> 不再依赖/对齐任何外部 CLI（agent-browser 已彻底移除）。

## 协议（指令集 v2）

> 与 [aic docs/instruction_sets_v2.md](../aic/docs/instruction_sets_v2.md) §6 对齐，
> 协议层实现见 `src/sdk/proto.js`（subject/信封/caps 纯函数，Go 侧 `libs/proto` 零漂移，
> 固定向量见 `src/sdk/proto.test.js` + `auth.test.js`）。

### 连接级 subject（host 生命周期）

| subject | 载荷 | 说明 |
|---|---|---|
| `u.{uid}.h.{host_id}.{cred_ver}.caps` | caps v2 JSON | 每次连接/重连发布，服务端以最近一次为准 |
| `u.{uid}.h.{host_id}.{cred_ver}.presence` | `{host_id, credential_ver, running, sent_at}` | 20s 心跳 |

### caps v2 声明（统一命令声明表，§6.3）

```json
{
  "host_id": "<host_id>",
  "credential_ver": 1,
  "agent_version": "v0.5.5",
  "device_type": "browser",
  "device_info": { "os": "Chrome", "arch": "browser", "num_cpu": 18 },
  "fs": { "actions": ["read", "write", "edit"] },
  "exec": {
    "commands": [
      { "name": "commands", "level": 1, "desc": "..." },
      { "name": "browser", "level": 2, "desc": "control a web browser (native; only on browser-extension/desktop hosts)", "help": "..." },
      { "name": "ls", "level": 1, "desc": "list directory entries", "help": "..." },
      { "name": "rg", "level": 1, "desc": "search content or list files", "help": "..." },
      { "name": "tree", "level": 1, "desc": "print directory tree (JSON)", "help": "..." },
      { "name": "rm", "level": 2, "desc": "remove files or directories", "help": "..." },
      { "name": "curl", "level": 2, "desc": "download a URL to a file", "help": "..." }
    ]
  }
}
```

- `fs.actions=[read/write/edit]`：扩展接入 PageFS（§4.5）——与 page 端同一套代码逻辑
  （`aic/ui/assets/libs/page_fs.js` ⇆ `src/sdk/page_fs.js` 逐字节同步），IndexedDB 单根；
  扩展与页面的 IndexedDB 因 origin 不同物理隔离，按 host_id 寻址。等级与 vcore.FSRequired
  同源（read=1，write/edit=2）；
- `exec.commands`：统一命令声明表（§5.1）——恒声明 `commands` + 注册命令 `browser`
  （required_level=2 Write，stateful 串行，与 Go vcore meta.go 同源）
  + fs 8 action（read/write/edit/ls/rg/cp/mv/rm，分级与 Go levels.go 对齐，操作扩展 PageFS）；
- §2.2 图片投递收敛：`browser screenshot` 落本 host 的 fs（`/screenshot/`，
  IndexedDB Blob 存储），不返回 image_data；agent 需要看图时用 `fs.read`（1host=本 host_id）
  按 attrs.path 读取——只有 fs.read 能把图片带进消息。

### 工具流量（§6.1 v4，subject 带 sid 段定向）

host 端连接时单订阅 `u.{uid}.h.host_{host_id}.>`（HostInboxSubject），
覆盖该 host 全部工具请求（fs/exec，7 段含 sid 段），sid 由信封 SessionID 携带，
无 per-session 订阅 churn。请求信封（server→host，HMAC-SHA256 签名，K_tool 派生）：

```json
{
  "msg_id": "...", "session_id": "...", "tool": "exec",
  "data": "{\"action\":\"browser\",\"argv\":[\"open\",\"https://...\"]}",
  "granted_level": 2, "nonce": "...", "deadline": "RFC3339", "sig": "..."
}
```

响应信封：`{msg_id, state: completed|waiting|rejected|error, content, error, attrs, need_approval}`。

host 端处理规范：验签 → deadline 过期拒绝 → nonce 窗口去重 → granted_level 纵深检查
（browser 指令 required=2，不足回 `waiting` 转人工审批）→ 分发。

## 插件设置页参数

| 参数 | 默认值 | 必填 | 说明 |
|---|---|---|---|
| `key` | | ✅ | AIC 环境凭证 |
| `host` | `https://ivec.ai` | | 平台地址（NATS 端点由此推断，与 cli/desktop 同一语义） |
| `background` | `true` | | 后台模式：AI 在独立标签页操作（普通窗口，共享登录态），创建 `active:false` 不抢焦点，绝不占用用户当前页面 |
| `incognito` | `false` | | 隐私模式：AI 在独立无痕窗口操作（不共享 cookie/登录态），与用户主窗口完全隔离 |
| `autoConnect` | `true` | | 启动后自动连接 |
| `viewport.width` | `1280` | | 默认视口宽度 |
| `viewport.height` | `720` | | 默认视口高度 |
| `timeout` | `30` | | 页面操作默认超时（秒） |

> **注意：** Chrome Extension 运行在用户浏览器内，因此不需要 `browserPath`（浏览器路径）、`userDataDir`（profile 目录）、`headless`（无头模式）等参数。`incognito=true` 通过 `chrome.windows.create({incognito: true})` 实现隔离（需在 chrome://extensions 为扩展开启「在无痕模式下启用」，否则创建无痕窗口失败并返回明确错误）。
>
> 以上参数不出现在 caps 声明和 exec 负载中。

## 工具定义

浏览器插件暴露 exec 虚拟指令 `browser`（调用形态 `exec browser <subcommand> [args...]`）。
**平台自有指令集**：实现 = `tools/browser/core.js`（平台无关）+ `chrome-adapter.js`（chrome.* 映射）；
desktop 端共享同一 core（Electron 适配器见 `desktop/electron-adapter.js`）。
caps 声明见上文（required_level=2，stateful）。

> **约定：** 目标页解析优先级（`background`/`incognito` 设置项）：`incognito=true` → 独立无痕窗口内 AI 标签页；`background=true` → 普通窗口内 AI 专属标签页；两者都关（协作模式）→ **当前活跃标签页**，需切换目标时先调用 `tab <N>` 切换。工作区模式下所有操作不激活 tab、不改变窗口焦点（executeScript/CDP 不要求 tab 激活），`tab` 系列限定在 AI 工作区窗口内；screenshot 经 CDP `Page.captureScreenshot`（`captureVisibleTab` 只能截当前激活 tab）。工作区 tab 状态持久化 chrome.storage（SW 重启恢复，误关自动重建）。

## 子命令总览（真实支持集，13）

| 子命令 | argv | 说明 |
|---|---|---|
| `open` | `<url>` | 打开 URL（http/https，工作区 tab，不抢焦点） |
| `click` | `<sel\|@ref>` | 点击元素（CSS 选择器或 @ref） |
| `close` | (无) | 关闭工作区当前 tab（下一条指令自动重建；最后一张拒绝，非错误） |
| `download` | `<sel> <path>` | 点击元素触发下载（等待 30s） |
| `eval` | `<js>` `[--base64]` | CDP Runtime.evaluate（DevTools 语义：不受页面 CSP 限制，可 await Promise） |
| `get` | `<what> [sel]` | 页面信息：text/html/title/url/value/attr/count/box/styles |
| `network` | `[id\|requests] [--filter f] [--type t] [--method m] [--status n] [--limit n] [--clear]` | 网络请求列表/详情（页内 fetch/XHR 拦截器，环形 500 条） |
| `read` | `[url]` | 提取可读文本（省略 url = 当前页；上限 100KB 截断） |
| `screenshot` | `[--quality N]` | JPEG 截图落本 host fs `/screenshot/`（CDP Page.captureScreenshot；fs.read 读图） |
| `snapshot` | `[-i] [-c] [-d N] [-s sel]` | a11y 树快照（@ref 代次机制，见 §5.6.1/§5.6.2） |
| `tab` | `[new\|list\|close [N]\|N]` | 工作区内标签页管理（1 基序号，不激活不抢焦点） |
| `wait` | `<sel\|ms>` `[--url g] [--load l] [--fn js] [--text t] [--download f]` | 等待条件（默认 30s） |
| `sleep` | `<dur>` | 暂停（1s/500ms/2m） |

未列能力（type/fill/press/滚动/导航等）统一报 `unknown action`，用 `eval <js>` 注入 JS 替代
（如 `el.value=...; el.dispatchEvent(new Event('input'))`）。

## 统一 attrs

响应 `attrs` 基础字段：`{action: "<subcommand>"}`；各子命令追加专属字段
（`path`/`rows`/`truncated`/`closed` 等），遵循 `fs` / `exec` 现有约定。

---

## 目录结构

```
browser/
├── manifest.json                 # Chrome Extension 声明
├── icons/                        # 扩展图标
│   ├── icon16.png
│   ├── icon48.png
│   └── icon128.png
├── options/                      # 设置页面
│   ├── options.html
│   ├── options.css
│   └── options.js                # 读写 chrome.storage.local
│
├── src/
│   ├── background.js             # Service Worker 入口（注册 browser 虚拟指令 + 生命周期）
│   │
│   ├── lib/                      # 第三方库
│   │   └── nats/                 # @nats-io/nats-core (复制自 aic/ui)
│   │       └── ...
│   │
│   ├── sdk/                      # AIC 客户端 SDK（指令集 v2.5）
│   │   ├── proto.js              # 协议层：subject/信封/caps v2 纯函数（Go libs/proto 零漂移）
│   │   ├── proto.test.js         # subject/caps 固定向量（node --test）
│   │   ├── crypto.js             # HKDF + HMAC-SHA256 (Web Crypto API)
│   │   ├── auth.js               # 连接 token (e1.*) + 工具请求验签（v2 canonical 输入）
│   │   ├── auth.test.js          # 密钥派生/签名固定向量（与 Go vectors_test.go 同源）
│   │   ├── client.js             # NATS 连接 / caps v2 发布 / 连接级 inbox 订阅 / 分发
│   │   ├── client.test.js        # 客户端单测（node --test）
│   │   ├── fsops.js              # fs 指令集 ls/rg/cp/mv/rm（与 aic 前端逐字节同步）
│   │   ├── fsops.test.js         # fsops 测试（node --test，vcore 向量同源）
│   │   ├── page_fs.js            # PageFS（IndexedDB 单根，与 aic 前端逐字节同步）
│   │   ├── history.js            # 执行历史（IndexedDB 持久化）
│   │   ├── argv.js               # action+argv 双层解析
│   │   ├── argv.test.js          # 双层解析测试
│   │   └── storage.js            # chrome.storage.local 读写封装
│   │
│   ├── content/                  # content script（页面桥接）
│   │   ├── local-bridge.js       # /hosts 页 → background 的本地通道桥（__aic_local）
│   │   └── network-interceptor.js# 网络请求拦截（browser network 用；desktop 经 CDP 注入同源文件）
│   │
│   └── tools/
│       ├── browser.js            # 装配入口：createBrowserHandler(chromeAdapter)
│       └── browser/              # browser 指令实现（平台自有指令集）
│           ├── core.js           # 平台无关核心：解析/分发/@ref 代次/工作区状态机
│           ├── chrome-adapter.js # Chrome 宿主适配（tabs/scripting/debugger/downloads/storage）
│           └── core.test.js      # mock-adapter 单测（node --test）
│
└── dist/                        # 构建产出 (make build)
    └── aic-browser.zip
```

### 依赖关系

```
background.js
  ├── sdk/client.js       → NATS 连接生命周期 + 连接级 inbox 分发
  │   ├── sdk/proto.js    → subject/信封/caps v2（纯函数）
  │   ├── sdk/crypto.js   → HKDF 密钥派生
  │   ├── sdk/auth.js     → e1 Token 生成 + 请求验签
  │   └── lib/nats/*.js   → NATS WebSocket
  │
  └── tools/browser.js    → createBrowserHandler(createChromeAdapter())
      ├── tools/browser/core.js          → 平台无关核心（desktop 共享）
      └── tools/browser/chrome-adapter.js → chrome.* 映射
  └── sdk/storage.js      → 读取用户设置（经 adapter.settings）
```

### 关键差异（vs Go SDK）

| Go SDK | JS (Chrome Extension) |
|---|---|
| `crypto/hmac` + `golang.org/x/crypto/hkdf` | `SubtleCrypto.importKey` + `.deriveBits` + `.sign` |
| `os.Exec` + `setpgid` | `chrome.tabs` / `chrome.scripting` API |
| `os.ReadFile` / `os.WriteFile` | PageFS（IndexedDB Blob） |
| `time.Now()` | `Date.now()` |
| `net/http` listen | Service Worker 自带生命周期 |
| goroutine 并发 | `Promise.all` / async/await |

---

## 与 desktop 客户端对比

| 维度 | desktop (Electron + Go) | browser (Chrome Extension) |
|---|---|---|
| 运行时 | Electron 壳 + Go 后端子进程 | Service Worker + Extension APIs |
| 核心技术 | 共享 browser core + Electron CDP（webContents.debugger） | 共享 browser core + chrome.tabs/scripting/debugger |
| 核心工具 | exec（统一命令声明表 + 壳注册 browser）、fs | browser 指令 + fs 8 action（PageFS） |
| browser 接线 | 壳通道（127.0.0.1 TCP 换行 JSON + token）→ provider 注册进 caps | SW 内直接注册 |
| 输出重定向 | 写入系统临时目录日志文件 | browser screenshot 落 PageFS（/screenshot/），其余直接返回 content |
| 权限等级 | exec 分级（curl/json=2 起、browser=2），fs=1 | browser=2，fs read/ls/rg=1、write/edit/cp/mv/rm=2 |
| 密钥派生 | Go crypto/hmac + hkdf | Web Crypto API (SubtleCrypto) |
| NATS 连接 | nats.go | @nats-io/nats-core（bundled ESM） |
| 安装方式 | 安装包 / 二进制 | Chrome Web Store / 本地加载 |
