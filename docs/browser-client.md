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
│  - CAPS 发布 (仅 action + argv 参数定义)                │
│  - tool.*.req 订阅 → { action, argv } 分发             │
│  - 操作标签页 / 注入脚本 / 截图                         │
└───────────────────────────────────────────────────────┘
```

> **Tool 层只认 `{action, argv}`**，全局参数在插件设置页配置，不出现在工具调用中。

## 插件设置页参数

| 参数 | 默认值 | 必填 | 说明 |
|---|---|---|---|
| `key` | | ✅ | AIC 环境凭证 |
| `url` | `wss://ivec.ai/aic/api/nc` | | AIC 服务端地址 |
| `deviceName` | 系统 hostname | | 设备展示名称 |
| `background` | `true` | | 后台模式，新窗口不抢夺焦点 |
| `incognito` | `false` | | 隐私模式，使用无痕窗口（不共享 cookie/登录态） |
| `autoConnect` | `true` | | 启动后自动连接 |
| `viewport.width` | `1280` | | 默认视口宽度 |
| `viewport.height` | `720` | | 默认视口高度 |
| `timeout` | `30s` | | 页面操作默认超时 |

> **注意：** Chrome Extension 运行在用户浏览器内，因此不需要 `browserPath`（浏览器路径）、`userDataDir`（profile 目录）、`headless`（无头模式）等参数。`incognito=true` 通过 `chrome.windows.create({incognito: true})` 实现隔离。
>
> 以上参数不出现在 CAPS 声明和 tool_data 中。

## 工具定义

浏览器插件注册一个 `browser` 工具，与 `agent-browser` CLI 签名对齐。

```json
{
  "name": "browser",
  "description": "Control a web browser via Chrome Extension APIs. Actions: open, click, close, dblclick, download, eval, get, network, read, screenshot, snapshot, tab, wait, scroll, hover, fill, press, select, back, forward, reload, sleep, cookies, storage, pipeline.",
  "parameters": {
    "type": "object",
    "properties": {
      "action": { "type": "string" },
      "argv": { "type": "array", "items": { "type": "string" } }
    },
    "required": ["action", "argv"]
  },
  "required_level": 1,
  "policy_version": "1"
}
```

> **约定：** 所有操作默认针对**当前活跃标签页**。需切换目标时，先调用 `tab <N>` 切换，后续操作即作用于新 tab。

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
| `screenshot` | 1 | `<filename>` | 截图 jpeg 80，必须带文件名 |
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
screenshot <filename> [options]
```
| argv | 类型 | 必填 | 说明 |
|---|---|---|---|
| `<filename>` | string | ✅ | 文件名，如 `screenshot/page.jpg` |
| `--format` | enum | | `png` / `jpeg` (默认 jpeg) |
| `--quality` | int | | jpeg 质量 1-100（默认 80） |
| `--full` | flag | | 全页面截图 |

默认 jpeg 质量 80。无 filename 时返回 JSON `{filename, data}` (base64)，此为空参后备行为。

**示例:** `["screenshot/page.jpg"]`, `["--full"]`, `["--format","png"]`

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
│   ├── background.js             # Service Worker 入口
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
│   ├── sdk/                      # AIC 客户端 SDK
│   │   ├── crypto.js             # HKDF + HMAC-SHA256 (Web Crypto API)
│   │   ├── auth.js               # Token 生成 (e1.*)
│   │   ├── client.js             # NATS 连接 / CAPS 发布 / 心跳 / 重连
│   │   ├── argv.js               # action+argv 双层解析
│   │   └── storage.js            # chrome.storage.local 读写封装
│   │
│   └── tools/                    # 工具实现
│       ├── browser.js            # open/click/close/download/eval/get/network/read/screenshot/snapshot/tab/wait/sleep 等核心 action
│       └── registry.js           # 工具注册表 (name → handler 映射)
│
└── dist/                        # 构建产出 (make build)
    └── aic-browser.zip
```

### 依赖关系

```
background.js
  ├── sdk/client.js       → NATS 连接生命周期
  │   ├── sdk/crypto.js   → HKDF 密钥派生
  │   ├── sdk/auth.js     → e1 Token 生成
  │   └── lib/nats/*.js   → NATS WebSocket
  │
  ├── sdk/handler.js      → 验签 + 工具分发
  │   └── tools/registry.js
  │       └── tools/browser.js
  │
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
| 核心工具 | `exec`, `fs` | `tabs`, `page`, `network`, `storage` |
| 输出重定向 | 写入系统临时目录日志文件 | 截图 data URL，其余直接返回 content |
| 权限等级 | exec=2, fs=1 | tabs=1, page=1, network=1, storage=2 |
| 密钥派生 | Go crypto/hmac + hkdf | Web Crypto API (SubtleCrypto) |
| NATS 连接 | nats.go | nats.ws (或直接 WebSocket + 协议实现) |
| 安装方式 | 二进制下载 | Chrome Web Store / 本地加载 |
