# 设备执行、文件与 MCP 服务

MCP 仅为 Pod 内部的一个 vsh command。平台 exec/fs 直接调用执行器和文件服务，UI/page/cloud 不实现 MCP。该 command 连接默认配置的官方 browser/CUA 和 `mcp.servers` 配置的第三方服务，没有平台 MCP server 或 exec、fs.* 的 MCP 注册。共享连接使用官方 Go SDK v1.7.0。

## 入口与路由

- AI 设置 `exec.1host=设备ID`，完整脚本经签名 NATS 到设备。脚本中的 mcp 命令只解析服务别名，无跨设备目标参数。
- UI 用 `$hosts.openTools(hostId)` 建立 RTC 连接，通过 `execCall(script, {stdin})` 调同一个 mcp command；JSON 参数经 stdin 传递，结果读取命令 stdout。
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

browser/CUA 默认配置无需手写：分别直接启动官方 `agent-browser mcp --tools core,tabs` 与 `cua-driver mcp`，复用现有 stdio manager。两项内置服务默认 30m `idle_timeout`（内置默认值改动只在本仓）；显式配置同名项会整体替换默认项，包括该默认有效期，不继承内置权限。Desktop 分发运行依赖；独立 CLI 按技能说明安装上游软件。`disabled: true` 无需填写 command。其他启用的服务 command 与 url 二选一；cwd 必须绝对路径。可设 disabled:true 或进程 no_sandbox:true。配置只由设备所有者维护，不从技能或工具参数加载。当前远程 HTTP 使用设备网络规则、不继承 shell env、不跟随重定向；没有 OAuth/token 管理器。

stdio 服务懒启动并复用一个 SDK session。进程由 vbox 负责；manager 只拥有配置与连接。一次取消不关闭服务；退出后的下一次显式调用可重新建连，失败写操作不自动重放。服务或启动权限配置更新时关闭旧连接，撤销相关临时服务授权。

`idle_timeout` 是每个服务条目的空闲有效期：服务启动或最后一次使用（取连接或在途请求结束）后经过该时长无任何调用即被回收，下次调用重新懒启动；在途请求算活跃，调用即续期，缺省空值表示随 Pod 常驻。回收是控制行为，不写故障日志（记一条生命周期日志）。内置 browser 的 MCP 连接与上游 daemon 分离——到期时显式用上游 CLI 关闭它自己的 daemon（连同受管浏览器），彻底释放资源；所有者覆盖的 browser 项自管 daemon，Pod 不代管。UI 实时画面挂载期间按分钟刷新 browser 的空闲截止时间（画面通道不经过 MCP session），避免观看中被回收。

调用门为 exec 规则 `mcp.<alias>`。内置 browser/CUA 以 Pod 设备权限运行，以访问 Chrome 私有状态和系统桌面；第三方服务默认受沙箱约束。进程 cwd/env/沙箱固定；按调用 session 获得的临时文件权限不扩展常驻进程沙箱。服务授权是设备能力授权，原生 fs 规则不会隔离浏览器或桌面操作。browser 默认用上游 `--filesystem-root` 限制显式文件参数为 Pod 工作目录；其余业务权限由上游服务负责。原生 exec 的 nosandbox 需可信审批；第三方 MCP 的 no_sandbox 只能由设备所有者在配置中设置。

exec 只接受完整 vsh 脚本；命令面 = vsh 默认 registry（`sed`/`grep`/`cut`/`sort` 等内建）+ 三个 contrib 命令（`jq`、`awk`、`html-to-markdown`）+ 本端平台命令（`mcp`/`ssh`/`scp`/`sftp`/`grant`/`bg`/`skill`）与端侧指令（`browser`/`cua`）。cloud（aic）与设备共用同一核心命令集（同一 `execution.NewRegistry()`）；`commands` 指令只展示平台自定义指令，常见命令不重复列出。

## 结果与边界

原生 RTC 请求与 NATS 消息上限 1 MiB。RTC 工具响应不超过 16 KiB 时为 JSON 文本，更大的响应使用 4 字节大端总长度头和连续的 16 KiB 二进制片段，完整 JSON 上限 16 MiB；并发响应按整包串行发送，认证文本不参与二进制重组。前端有界重组并校验 UTF-8，未完成响应超时或连接关闭时释放缓存；发送失败关闭连接，使待处理请求立即失败。此能力需前端与 Pod 同步升级。

mcp command 复用现有 exec 结果规则：AI 路径可返回预览与日志引用，RTC 直连返回结果，包括 CUA 截图的标准 MCP image 内容。引擎的 stdout/stderr 内存采集上限仍分别为 8 MiB、1 MiB；仅 RTC 前台执行完成后，对截断的流从本次执行自有日志中有界补全，最终响应仍受 16 MiB JSON 总上限约束。补全不重新执行命令；日志读取失败或结果超限明确返回错误并保留可用日志元数据，不返回半截成功结果。NATS 预览与日志引用规则保持不变。

`mcp call <server> <tool> --image-preview WIDTHxHEIGHT` 显式启用设备端图片预览，每边必须为 1..4096，仅 `call` 支持。成功结果中的 image 内容等比缩入指定尺寸、小图不放大，JPEG 质量从 70 起按需降低，单图不超过 600 KiB；上游 structuredContent、capture_id、annotations 与其他元数据保留，图片 `_meta["aic.dev/image-preview"]` 增加 `source_width/source_height/width/height`，供调用方还原上游截图坐标。CUA UI 使用 `1280x720`。工具错误结果不转换，压缩失败不重跑工具；普通调用默认透传，无需增加截图文件或文件授权。

大文件走 fs source/upload 分块。工具失败使用 isError 并退出非零，可保留部分结果。除显式图片预览外，工具名称、schema、content、structuredContent、isError 与 _meta 使用上游值。执行取消使用 cancel(request_id) 或 bg kill，MCP 协议细节由 Pod SDK 处理。

command 提供 tools/describe/call/read；不引入 prompts、sampling、任务引擎或技能依赖求解器。服务的业务权限仍由对应服务负责，MCP 传输本身不是沙箱。

Browser UI 调用上游工具并通过下述 `aic-browser` 通道查看与控制画面；CUA UI 使用截图预览和上游输入工具，并保留 `check_permissions({prompt:false})` 只读诊断。

依赖与用法见 [browser](../../aic-skills/browser/SKILL.md)、[CUA](../../aic-skills/cua/SKILL.md) 与 [构建发行](release-architecture.md)。

内建 Browser UI 的 `aic-browser` RTC data channel 复用同一 peer 的设备认证和 `mcp.browser` 命令权限，双向转发该 agent-browser daemon 的原生 WebSocket 消息。Pod 仅做分片传输，不解析浏览器工具、不建立 CDP 连接。UI 绘制帧后才把 ACK 转回上游；关闭此通道不影响命令连接。
