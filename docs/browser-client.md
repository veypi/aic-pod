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
│  - 操作标签页 / 注入脚本 / 截图                         │
└───────────────────────────────────────────────────────┘
```

> **协议（指令集 v2.6）：** 本扩展的 exec 命令表 = 恒声明 `commands`（能力发现）+ 注册命令
> `browser`（`exec browser <subcommand> [args...]`）；fs 能力 = 8 action
> （read/write/edit/ls/rg/cp/mv/rm，PageFS+fsops 后端，与 page 端逐字节同源）。
> 全局参数在插件设置页配置，不出现在工具调用中。

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
      { "name": "browser", "level": 2, "desc": "control a web browser (agent-browser CLI)", "help": "..." },
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
  （required_level=2 Write，stateful 串行，download/wait 可后台化，与 Go vcore meta.go 同源）
  + fs 8 action（read/write/edit/ls/rg/cp/mv/rm，分级与 Go levels.go 对齐，操作扩展 PageFS）；
- §2.2 图片投递收敛：`browser screenshot` 落本 host 的 fs（`$SESSION/screenshot/`，
  IndexedDB Blob 存储），不返回 image_data；agent 需要看图时用 `fs.read`（1host=本 host_id）
  按 attrs.path 读取——只有 fs.read 能把图片带进消息。

### 工具流量（§6.1 v4，subject 带 sid 段定向）

host 端连接时单订阅 `u.{uid}.h.host_{host_id}.>`（HostInboxSubject），
覆盖该 host 全部工具请求（fs/exec，7 段含 sid 段），sid 由信封 SessionID 携带，
无 per-session 订阅 churn。请求信封（server→host，HMAC-SHA256 签名，K_tool 派生）：

```json
{
  "msg_id": "...", "session_id": "...", "tool": "exec",
  "data": "{"action":"browser","argv":["open","https://..."]}",
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

浏览器插件暴露 exec 虚拟指令 `browser`（调用形态 `exec browser <subcommand> [args...]`），
子命令与 `agent-browser` CLI 签名对齐。caps 声明见上文（required_level=2，stateful，backgroundable）。

> **约定：** 目标页解析优先级（`background`/`incognito` 设置项）：`incognito=true` → 独立无痕窗口内 AI 标签页；`background=true` → 普通窗口内 AI 专属标签页；两者都关（协作模式）→ **当前活跃标签页**，需切换目标时先调用 `tab <N>` 切换。工作区模式下所有操作不激活 tab、不改变窗口焦点（executeScript/CDP 不要求 tab 激活），`tab` 系列限定在 AI 工作区窗口内；screenshot 经 CDP `Page.captureScreenshot`（`captureVisibleTab` 只能截当前激活 tab）。工作区 tab 状态持久化 chrome.storage（SW 重启恢复，误关自动重建）。

## action 总览

| action | level | argv | 说明 |
|---|---|---|---|
| `open` | 1 | `<url>` | 打开 URL |
| `click` | 1 | `<sel>` | 点击元素（CSS 选择器或 @ref） |
| `close` | 1 | (无) | 关闭当前活跃标签页 |
| `dblclick` | 1 | `<sel>` | 双击元素 |
| `download` | 1 | `<sel> <path>` | 点击元素触发下载 |
| `eval` | 2 | `<js>` | 执行 JavaScript |
| `get` | 1 | `<what> [sel]` | 获取页面信息 |
| `network` | 1 | `[id\|--filter ...]` | 网络请求 |
| `read` | 1 | `[url]` | 提取页面可读文本 |
| `screenshot` | 1 | `[--quality N] [--full]` | 截图落 host fs（$SESSION/screenshot/），fs.read 读图 |
| `snapshot` | 1 | `[-i] [-c] [-d N] [-s sel]` | a11y tree 快照 |
| `tab` | 1 | `<new\|list\|close\|N>` | 标签页管理 |
| `wait` | 1 | `<sel\|ms\|--option>` | 等待条件 |
| `scroll` | 1 | `<dir> [px]` | 滚动页面 |
| `hover` | 1 | `<sel>` | 悬停元素 |
| `fill` | 1 | `<sel> <text>` | 填写输入框 |
| `press` | 1 | `<key>` | 按键 |
| `select` | 1 | `<sel> <value>` | 下拉框选择 |
| `back` | 1 | (无) | 后退 |
| `forward` | 1 | (无) | 前进 |
| `reload` | 1 | (无) | 刷新页面 |
| `sleep` | 1 | `<duration>` | 暂停 |
| `cookies` | 2 | `<get\|set\|clear> ...` | Cookie 管理 |
| `storage` | 2 | `<local\|session> <get\|set\|del> ...` | Web Storage 管理 |
| `pipeline` | 1 | `<action> && <action> ...` | 链式执行多个操作 |

## action 参数详表

每个 action 的 argv 格式与 `agent-browser` CLI 对齐。支持 `--timeout N` (ms) 全局 flag。

### open
```
open <url>
```
| argv | 类型 | 必填 | 说明 |
|---|---|---|---|
| `<url>` | string | ✅ | 完整 URL |

**示例:** `["https://github.com"]`

### click
```
click <sel>
```
| argv | 类型 | 必填 | 说明 |
|---|---|---|---|
| `<sel>` | string | ✅ | CSS 选择器或 @ref（来自 snapshot） |

**示例:** `["@e2"]`, `["button.submit"]`, `["#login-btn"]`

### dblclick
```
dblclick <sel>
```
| argv | 类型 | 必填 | 说明 |
|---|---|---|---|
| `<sel>` | string | ✅ | CSS 选择器或 @ref |

**示例:** `["@e3"]`, `["tr.selected"]`

### close
无参数。关闭当前活跃标签页。最后一个标签页不可关闭。

### download
```
download <sel> <path>
```
| argv | 类型 | 必填 | 说明 |
|---|---|---|---|
| `<sel>` | string | ✅ | CSS 选择器或 @ref，指向触发下载的元素 |
| `<path>` | string | ✅ | 相对路径，保存到默认下载目录 |

通过点击元素触发浏览器下载，等待下载完成后返回文件路径。

**示例:** `["@e5", "report.xlsx"]`, `["#dl-btn", "file.pdf"]`

### eval
```
eval <js> [-b]
```
| argv | 类型 | 必填 | 说明 |
|---|---|---|---|
| `<js>` | string | ✅ | JavaScript 代码 |
| `-b` | flag | | `<js>` 为 base64 编码 |

**示例:** `["document.title"]`, `["-b", "ZG9jdW1lbnQudGl0bGU="]`

### get
```
get <what> [selector]
```
| argv | 类型 | 必填 | 说明 |
|---|---|---|---|
| `<what>` | enum | ✅ | `text`, `html`, `title`, `url`, `value`, `attr <name>`, `count`, `box`, `styles`, `cdp-url` |
| `[sel]` | string | | CSS 选择器（部分 what 需要） |

**示例:** `["title"]`, `["text"]`, `["value", "@e1"]`, `["attr", "href", "@e5"]`, `["count", "a"]`

### network
```
network [id|--filter ...]
```
| argv | 类型 | 必填 | 说明 |
|---|---|---|---|
| `<reqid>` | int | | 请求 ID（查看详情），省略则列出 |
| `--filter` | string | | URL 子串过滤 |
| `--type` | string | | 逗号分隔，如 `xhr,fetch` |
| `--method` | string | | HTTP 方法过滤 |
| `--status` | int | | HTTP 状态码过滤 |
| `--clear` | flag | | 清空记录 |

**示例:** `[]`列表, `["--filter","api"]`, `["req-123"]`, `["--type","xhr,fetch","--status","200"]`

### read
```
read [url]
```
| argv | 类型 | 必填 | 说明 |
|---|---|---|---|
| `[url]` | string | | 目标 URL，省略则读取当前页面 |

提取页面可读文本（去噪后的主要内容）。

**示例:** `[]`, `["https://example.com"]`

### screenshot
```
screenshot [--quality N] [--full]
```
| argv | 类型 | 必填 | 说明 |
|---|---|---|---|
| `--quality` | int | | jpeg 质量 1-100（默认 80） |
| `--full` | flag | | 全页面截图 |

截图（jpeg）落本 host 的 fs：`$SESSION/screenshot/screenshot-{ts}.jpg`（扩展 IndexedDB
Blob 存储），content/attrs.path 告知落盘位置。不返回图片数据（§2.2）——agent 需要看图时
用 `fs.read`（1host=本 host_id）读取。

**示例:** `[]`, `["--full"]`, `["--quality","60"]`

### snapshot
```
snapshot [-i] [-c] [-d N] [-s sel]
```
| argv | 类型 | 必填 | 说明 |
|---|---|---|---|
| `-i` | flag | | 仅交互元素 |
| `-c` | flag | | 紧凑模式（省略 StaticText） |
| `-d` | int | | 深度限制（默认无限制） |
| `-s` | string | | 限定 CSS 选择器范围 |

**content:** 每行 `uid=snapshotId_seq  role  ["name"]  [attr]`，缩进表层级。@ref 用于 click/fill/hover。

**示例输出:**
```
uid=3_0 RootWebArea "GitHub"
  uid=3_1 link "Skip to content"
  uid=3_2 button "Search" haspopup="menu"
  uid=3_3 heading "Repository"
  uid=3_4 link "veypi/aic-pod"
  uid=3_5 textbox focused value="hello"
```

### tab
```
tab <new|list|close|N>
```
| argv | 类型 | 必填 | 说明 |
|---|---|---|---|
| `<action>` | enum | ✅ | `new` 新建, `list` 列表, `close` 关闭, `<N>` 切换到第 N 个 |

**示例:** `["new"]`, `["list"]`, `["close"]`, `["0"]`

### wait
```
wait <sel|ms|--option>
```
| argv | 类型 | 必填 | 说明 |
|---|---|---|---|
| `<sel\|ms>` | string | ✅ | CSS 选择器等待出现，或毫秒数 |
| `--url` | glob | | 等待 URL 匹配 |
| `--load` | enum | | `networkidle`, `domcontentloaded` |
| `--fn` | string | | 等待 JS 表达式为真 |
| `--text` | string | | 等待文本出现 |

**示例:** `["@e1"]`, `["2000"]`, `["--load","networkidle"]`, `["--text","Welcome"]`

### scroll
```
scroll <dir> [px]
```
| argv | 类型 | 必填 | 说明 |
|---|---|---|---|
| `<dir>` | enum | ✅ | `up`, `down`, `left`, `right` |
| `[px]` | int | | 像素数，默认一屏 |

**示例:** `["down"]`, `["down","300"]`

### hover
```
hover <sel>
```
| argv | 类型 | 必填 | 说明 |
|---|---|---|---|
| `<sel>` | string | ✅ | CSS 选择器或 @ref |

### fill
```
fill <sel> <text>
```
| argv | 类型 | 必填 | 说明 |
|---|---|---|---|
| `<sel>` | string | ✅ | CSS 选择器或 @ref |
| `<text>` | string | ✅ | 填充内容（先清空再输入） |

**示例:** `["@e5", "hello@example.com"]`, `["#search", "query"]`

### press
```
press <key>
```
| argv | 类型 | 必填 | 说明 |
|---|---|---|---|
| `<key>` | string | ✅ | `Enter`, `Tab`, `Escape`, `ArrowDown`, `Control+a` 等 |

### select
```
select <sel> <value>
```
| argv | 类型 | 必填 | 说明 |
|---|---|---|---|
| `<sel>` | string | ✅ | CSS 选择器或 @ref，指向 `<select>` 元素 |
| `<value>` | string | ✅ | 选项的 value 或显示文本 |

**示例:** `["#country", "CN"]`, `["@e8", "Option 1"]`

### back
无参数。页面向后导航（`history.back()`）。

### forward
无参数。页面前进导航（`history.forward()`）。

### reload
无参数。刷新当前页面。

### sleep
```
sleep <duration>
```
| argv | 类型 | 必填 | 说明 |
|---|---|---|---|
| `<duration>` | string | ✅ | `1s`, `500ms`, `2s` 等 |

### pipeline
```
pipeline <action> && <action> [&& ...]
```
| argv | 类型 | 必填 | 说明 |
|---|---|---|---|
| `<action>` | string | ✅ | 各 action 及其参数，以 `&&` 分隔 |

一次调用按顺序执行多个操作，步骤间默认延迟 1s。遇到错误立即停止。显式 `sleep` 步骤可覆盖默认延迟。

**示例:** `["open", "https://example.com", "&&", "snapshot", "-i", "&&", "click", "@e1"]`

### cookies
```
cookies <get|set|clear> [args]
```
| 子命令 | argv | 说明 |
|---|---|---|
| `get` | `[--name N] [--domain D]` | 查询 cookie |
| `set` | `--name N --value V [--domain D] [--path /] [--httpOnly] [--secure]` | 设置 cookie |
| `clear` | `[--name N] [--domain D]` | 清除 cookie |

**示例:** `["get"]`, `["set","--name","token","--value","abc"]`, `["clear","--name","session"]`

### storage
```
storage <local|session> <get|set|del> [args]
```
| 子命令 | argv | 说明 |
|---|---|---|
| `get` | `--key K` | 读取 |
| `set` | `--key K --value V` | 写入 |
| `del` | `--key K` | 删除 |

**示例:** `["local","get","--key","theme"]`, `["session","set","--key","step","--value","1"]`

## 统一 attrs

所有响应 `attrs` 基础字段：

```json
{
  "action": "click",
  "tab_id": "42",
  "url": "https://..."
}
```

各 action 追加专属字段。遵循 `fs` / `exec` 现有约定：`rows`, `truncated`, `path` 等语义保持一致。

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
│   │       ├── nats-core.js
│   │       ├── nats-core-internal.js
│   │       ├── nuid.js
│   │       ├── nkeys.js
│   │       ├── js-sha256.js
│   │       ├── tweetnacl.js
│   │       ├── kv.js
│   │       ├── obj.js
│   │       ├── services.js
│   │       ├── jetstream.js
│   │       └── jetstream-internal.js
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
│   │   └── network-interceptor.js# 网络请求拦截（browser network 用）
│   │
│   └── tools/                    # 虚拟指令实现
│       └── browser.js            # exec browser 子命令实现（open/click/snapshot/...）
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
  └── tools/browser.js    → exec browser 子命令实现
  └── sdk/storage.js      → 读取用户设置
```

### 关键差异（vs Go SDK）

| Go SDK | JS (Chrome Extension) |
|---|---|
| `crypto/hmac` + `golang.org/x/crypto/hkdf` | `SubtleCrypto.importKey` + `.deriveBits` + `.sign` |
| `os.Exec` + `setpgid` | `chrome.tabs` / `chrome.scripting` API |
| `os.ReadFile` / `os.WriteFile` | 无本地文件系统，`fetch` + 截图 data URL |
| `time.Now()` | `Date.now()` |
| `net/http` listen | Service Worker 自带生命周期 |
| goroutine 并发 | `Promise.all` / async/await |

---

## 与 desktop 客户端对比

| 维度 | desktop (Go) | browser (Chrome Extension) |
|---|---|---|
| 运行时 | 独立二进制 | Service Worker + Extension APIs |
| 核心技术 | Go SDK | JS + chrome.tabs / chrome.scripting / chrome.cookies / ... |
| 核心工具 | exec（统一命令声明表）、fs | browser 指令 + fs 8 action（PageFS） |
| 输出重定向 | 写入系统临时目录日志文件 | browser screenshot 落 PageFS（$SESSION/screenshot/），其余直接返回 content |
| 权限等级 | exec 分级（curl/json=2 起、browser=2），fs=1 | browser=2，fs read/ls/rg=1、write/edit/cp/mv/rm=2 |
| 密钥派生 | Go crypto/hmac + hkdf | Web Crypto API (SubtleCrypto) |
| NATS 连接 | nats.go | @nats-io/nats-core（bundled ESM） |
| 安装方式 | 二进制下载 | Chrome Web Store / 本地加载 |
