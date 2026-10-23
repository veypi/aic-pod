# 设备执行、文件与工具服务

MCP 只是服务协议之一（host-vtools：通用工具服务注册）。服务（MCP/TCP/…）经 libs/vtools 适配后注册为 vsh 根命令，工具调用直达（`<alias> <tool> [flags]`）；`mcp` 命令退化为管理面（list/add/remove/start/stop + tools/describe/call/read 调试直通）。平台 exec/fs 直接调用执行器和文件服务，UI/page/cloud 不实现 MCP。命令连接默认配置的官方 browser/CUA 和 `mcp.servers` 配置的第三方服务，没有平台 MCP server 或 exec、fs.* 的 MCP 注册。共享连接使用官方 Go SDK v1.7.0。

本文含两部分：设备执行与工具服务的**现行契约**（入口与路由、配置与运行、结果与边界），以及「执行边界」速查（vsh 与 MCP 的分工）。

## 入口与路由

- AI 设置 `exec.1host=设备ID`，完整脚本经签名 NATS 到设备。脚本中的服务命令只解析本机服务别名，无跨设备目标参数。
- UI 用 `$hosts.openTools(hostId)` 建立 RTC 连接，通过 `execCall(script, {stdin})` 调同一套 vsh 命令；JSON 参数经 stdin 传递，结果读取命令 stdout。
- 文件 HTTP proxy 只转发原生 fs 请求，设备再次限制 scope=fs。

NATS `hosts_nats/3` 签名覆盖 host、subject、caller、origin、scope、grant_approved、nonce、deadline、authorization_until 和原生 request。request 包含 protocol、request_id、action 与对应 exec/fs/cancel_id 载荷；action 仅 exec/fs/cancel。响应包含 protocol、request_id、result 和 error，可同时保留错误与部分日志。

RTC `hosts_rtc/3` 使用可靠有序的 `aic-tools` DataChannel。每帧只允许以下一种载荷：

- 身份验证：`{id,auth:"open"|"renew",ticket}`，票据绑定设备、用户与 DTLS 指纹。
- 原生操作：`{tool:{protocol,request_id,action,exec?|fs?|cancel_id?,timeout_ms?},grant_approved?}`。审批事实只用于 exec。

取消按 request_id 找到同用户、同会话的执行。mcp command 复用执行上下文，由 Pod SDK 将取消传给服务；没有独立的 RTC MCP 分支或取消协议。

## 配置与运行

```yaml
mcp:
  servers:
    browser:
      disabled: true  # 可选：禁用默认内置服务
    local:
      command: /absolute/path/custom-server
      cwd: /absolute/workdir
      idle_timeout: 30m  # 可选：空闲有效期，缺省空 = 随 Pod 常驻
    remote:
      url: https://service.example/mcp
```

browser/CUA/blender 是内置服务，默认项由 aic-skills 对应包的 `cli/manifest.json` 声明（pod 经 embed 读取，不再硬编码）：分别启动官方 `agent-browser mcp --tools core,tabs` 与 `cua-driver mcp`，blender 是 `tcp://127.0.0.1:9866` 的 addon 通道（含 doctor/launch/install 生命周期动词与分级报错，详见 blender 技能手册）。browser/CUA 默认 30m `idle_timeout`；所有者显式配置同名项会整体替换内置项（含默认有效期与权限），`disabled: true` 关闭。Desktop 分发运行依赖；独立 CLI 按技能说明安装上游软件。`disabled: true` 无需填写 command。其他启用的服务 command 与 url 二选一；cwd 必须绝对路径。可设 disabled:true 或进程 no_sandbox:true。url 另接受 `tcp://host:port`（vtools/tcp v1 行分隔 JSON 协议——非 MCP 服务，由注册层直接接管，不进连接管理器）。配置只由设备所有者维护，不从技能或工具参数加载；亦可用 `mcp add/remove [--persist]` 在会话里增删（需 exec 规则放行 `mcp`；不带 --persist 进程级生效）。当前远程 HTTP 使用设备网络规则、不继承 shell env、不跟随重定向；没有 OAuth/token 管理器。

stdio 服务懒启动并复用一个 SDK session。进程由 vbox 负责；manager 只拥有配置与连接。一次取消不关闭服务；退出后的下一次显式调用可重新建连，失败写操作不自动重放。服务或启动权限配置更新时关闭旧连接，撤销相关临时服务授权。

`idle_timeout` 是每个服务条目的空闲有效期：服务启动或最后一次使用（取连接或在途请求结束）后经过该时长无任何调用即被回收，下次调用重新懒启动；在途请求算活跃，调用即续期，缺省空值表示随 Pod 常驻。回收是控制行为，不写故障日志（记一条生命周期日志）。内置 browser 的 MCP 连接与上游 daemon 分离——到期时显式用上游 CLI 关闭它自己的 daemon（连同受管浏览器），彻底释放资源；所有者覆盖的 browser 项自管 daemon，Pod 不代管。UI 实时画面挂载期间按分钟刷新 browser 的空闲截止时间（画面通道不经过 MCP session），避免观看中被回收。

调用门为 exec 规则 `mcp.<alias>`（服务根命令边界统一检查；`grant cmd mcp.<alias>` 放行该服务全部工具与流）。内置 browser/CUA 以 Pod 设备权限运行，以访问 Chrome 私有状态和系统桌面；第三方服务默认受沙箱约束。进程 cwd/env/沙箱固定；按调用 session 获得的临时文件权限不扩展常驻进程沙箱。服务授权是设备能力授权，原生 fs 规则不会隔离浏览器或桌面操作。browser 默认用上游 `--filesystem-root` 限制显式文件参数为 Pod 工作目录；其余业务权限由上游服务负责。原生 exec 的 nosandbox 需可信审批；第三方 MCP 的 no_sandbox 只能由设备所有者在配置中设置。

exec 只接受完整 vsh 脚本；命令面 = vsh 默认 registry（`sed`/`grep`/`cut`/`sort` 等内建）+ 三个 contrib 命令（`jq`、`awk`、`html-to-markdown`）+ 本端平台命令（`mcp`/`ssh`/`scp`/`sftp`/`grant`/`bg`/`pull`/`push`）+ 已登记的服务根命令（内置 `browser`/`cua`/`blender` 与 `mcp add` 添加的服务；`<alias>` 无参列工具目录，`<alias> <tool> --help` 看参数表）。cloud（aic）与设备共用同一核心命令集（同一 `execution.NewRegistry()`）；`commands` 指令只展示平台自定义指令与已登记服务，常见命令不重复列出。（2026-10-12：`skill` 命令在设备端退役——包/文件拉取改用 `pull`，走 /fs/cloud HTTP 面。）

## 结果与边界

原生 RTC 请求与 NATS 消息上限 1 MiB。RTC 工具响应不超过 16 KiB 时为 JSON 文本，更大的响应使用 4 字节大端总长度头和连续的 16 KiB 二进制片段，完整 JSON 上限 16 MiB；并发响应按整包串行发送，认证文本不参与二进制重组。前端有界重组并校验 UTF-8，未完成响应超时或连接关闭时释放缓存；发送失败关闭连接，使待处理请求立即失败。此能力需前端与 Pod 同步升级。

服务命令复用现有 exec 结果规则：AI 路径可返回预览与日志引用，RTC 直连返回结果，包括 CUA 截图的标准 MCP image 内容。引擎的 stdout/stderr 内存采集上限仍分别为 8 MiB、1 MiB；仅 RTC 前台执行完成后，对截断的流从本次执行自有日志中有界补全，最终响应仍受 16 MiB JSON 总上限约束。补全不重新执行命令；日志读取失败或结果超限明确返回错误并保留可用日志元数据，不返回半截成功结果。NATS 预览与日志引用规则保持不变。

`mcp call <server> <tool> --image-preview WIDTHxHEIGHT`（调试直通）与服务根命令 `<alias> <tool> --image-preview` 均显式启用设备端图片预览，每边必须为 1..4096。成功结果中的 image 内容等比缩入指定尺寸、小图不放大，JPEG 质量从 70 起按需降低，单图不超过 600 KiB；上游 structuredContent、capture_id、annotations 与其他元数据保留，图片 `_meta["aic.dev/image-preview"]` 增加 `source_width/source_height/width/height`，供调用方还原上游截图坐标。CUA UI 使用 `1280x720`。工具错误结果不转换，压缩失败不重跑工具；普通调用默认透传，无需增加截图文件或文件授权。

大文件走 fs source/upload 分块。工具失败使用 isError 并退出非零，可保留部分结果。除显式图片预览外，工具名称、schema、content、structuredContent、isError 与 _meta 使用上游值。执行取消使用 cancel(request_id) 或 bg kill，MCP 协议细节由 Pod SDK 处理。

`mcp` 管理面提供 list/add/remove/start/stop/restart 与调试直通 tools/describe/call/read；不引入 prompts、sampling、任务引擎或技能依赖求解器。服务的业务权限仍由对应服务负责，MCP 传输本身不是沙箱。

Browser UI 调用上游工具并通过下述 `aic-browser` 通道查看与控制画面；CUA UI 使用截图预览和上游输入工具，并保留 `check_permissions({prompt:false})` 只读诊断。

依赖与用法见 [browser](../../aic-skills/browser/SKILL.md)、[CUA](../../aic-skills/cua/SKILL.md) 与 [构建发行](release-architecture.md)。

内建 Browser UI 的 `aic-browser` RTC data channel 复用同一 peer 的设备认证和 `mcp.browser` 命令权限，双向转发该 agent-browser daemon 的原生 WebSocket 消息。Pod 仅做分片传输，不解析浏览器工具、不建立 CDP 连接。UI 绘制帧后才把 ACK 转回上游；关闭此通道不影响命令连接。（2026-10-16 起该通道走通用流注册：rtc.Config.Streams 按 DataChannel 标签现查 vtools 登记表，任何实现 vtools.Streamer 的服务都能获得同形态通道；见 host-vtools.md §4。）

## 执行边界（vsh 与 MCP）

现行实现：2026-10-12（原文为独立设计稿 hosts-vsh-redesign.md，现并入本文）。

- 外层 `exec.1host` 唯一选择执行端；mcp 命令不再选择目标。
- cloud/设备执行完整 vsh 脚本。内建、CLI、脚本、管道、重定向和 bg 使用现有执行器，原生程序经 vbox 启动。
- 设备 `mcp tools/describe/call/read` 只接配置的独立服务，不按技能注册命令、不生成 flags；参数使用 JSON，stdin 可传 `--input -`。
- page 接受一个字面 command，按 session 和现有窗口 ID 调用 handler；通过 commands 和 --help 发现用法。复用 vsh 语法解析器，不建立另一套 shell，也不实现 MCP。页面文件继续直接调用 PageFS。
- exec/fs、技能搜索/读取、云端 HTTP API 都没有 MCP 副本。Cloud 当前没有 MCP 服务配置或 mcp 命令。
- 服务门为 `mcp.<alias>`；共享服务的 cwd/env/沙箱来自设备配置，不随某次 shell 改变。配置更新或 Pod 退出时回收服务，单次取消不关闭整个服务。
- UI 与 AI 都按原生 request_id 取消执行，Pod 内 SDK 将上下文取消传给 MCP 服务。取消不回滚副作用；响应丢失不自动重放。
- `skill search/load` 读取云端静态内容，fork 创建副本（均 cloud 端接线）。安装脚本、CLI 或 MCP 程序按说明执行。
- **设备凭证 = vbase 静态秘钥**（device-key-migration 2026-10-12）：config `key` 存复合凭证 `<host_id>.<vbase 四段 credential>`；NATS CONNECT token 与 HTTP Bearer 均为四段 credential；第 4 段 secret 是本地 HKDF 根（K_tool/RTC 票据不变）。e1. 自签连接 proof、nonce 防重放、enc1. 存储、credVer 与 NATS fetch 分块通道全删。
- **pull/push**（设备端平台命令）：设备 ↔ 平台云盘走 /fs/cloud HTTP 面——`pull <云端路径> [本地路径]`（目录递归：一次 ?depth=5 列表 + 逐文件 GET）、`push <本地路径> <云端路径>`（目录 v1 拒绝）；Bearer 由 pod 进程注入不进沙箱 env；非 loopback 的 http:// 拒绝发凭证；exec 规则 `pull`/`push` 门（默认开，锁 deny 的设备用 `grant cmd pull` 收放）。

旧技能 provider、设备安装表、包根命令、通用媒体流、host 端 skill 命令与 fetch 通道已删除，没有兼容入口。
