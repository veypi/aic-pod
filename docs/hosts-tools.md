# 设备能力统一协议与实现

更新：2026-09-28（hosts-vsh-redesign 落地）。详细设计见 [hosts-vsh-redesign](hosts-vsh-redesign.md)。平台、设备和前端需要共同升级，没有旧协议兼容入口。

## 总览

AI 只有内建 `fs` 和 `exec` 两个工具。exec 是唯一执行动作：一段 vsh 脚本。命令发现走脚本内 `commands` + `<cmd> --help`；后台走脚本内 `bg`；授权走脚本内 `grant`。browser/cua 是 vsh 指令（注册进引擎 Registry），不再是独立的 wire 命令；原生程序不逐个注册——Registry 未命中时按命令规则合成执行，实际进程由 OS 沙箱约束。

- `protocol/hosts_tools`（hosts_tools/2）定义三个动作（exec/fs/cancel）、统一调用体、调用者身份（含 grant_approved）与错误。
- `protocol/hosts_nats`（hosts_nats/2）是服务端可信转发信封：HMAC 签名携带 caller、归属会话与 grant_approved 审批事实；Verify 强制 caller 与路由 subject 中的 uid 一致（执行/任务/取消归属统一从 caller 派生，信封身份不能与路由归属脱节）。
- `protocol/hosts_rtc`（hosts_rtc/2）是 owner 前端直连：票据绑定 DTLS 指纹与归属会话，grant_approved 是逐请求的确认元信息。
- `libs/vsh` 是 vsh 引擎装配层：可信执行上下文（归属/grant_approved/nosandbox 经 context 注入，不从脚本可修改的 argv/env 读取）、统一外层 Execute（前台等待 + 超时登记后台）、平台命令（commands/bg/grant/list_hosts/send_user）。
- `libs/hostauth` 负责 RTC 票据、DTLS 身份绑定和续期。
- `vbox`（外部依赖 ivec/vbox）托管原生进程：OS 沙箱 profile 一律由规则表派生（数字等级已删除），免沙箱只来自可信上下文的 nosandbox。
- `libs/hostfs` 拥有路径、版本、上传和字节源；AI 文本操作与前端二进制文件操作使用同一 FS 实现。
- `libs/browser` / `libs/cua` 拥有 Chrome/窗口/快照，经 `VshCommand` 暴露为 vsh 指令；`page.frames`/`page.input` 是 RTC 私有 stream 端点，不注册为指令。
- `ui/1` 保留为 cua 内部操作与结果词汇（target/snapshot/ref 等），不是独立的协议入口；对外统一经 hosts_tools/2 与 vsh 指令。

## 审批模型

审批只留两处，且全部在发送前完成：

1. 脚本含字面 `grant`（analyze 静态检出，含 eval/source 展开）→ 必须人工批准；
2. `nosandbox: true` → 必须人工批准。

批准后服务端把 `grant_approved: true` 写入签名信封（NATS）或逐请求确认元信息（RTC）。pod 不重新分类审批、不返回 waiting/approval_required；pod 侧唯一边界是引擎内 `grant` 命令检查可信上下文的 grant_approved——缺失即拒绝并引导重新审批。规则表（fs/net/exec 四域）在执行点即时生效，不属于审批。

## 调用

普通请求三个传输同一形状（`Request`）：

```json
{
  "protocol": "hosts_rtc/2",
  "request_id": "r_example",
  "action": "exec",
  "exec": {"script": "browser page.create 'https://example.com' --json", "wait_ms": 30000},
  "timeout_ms": 35000
}
```

- `action: "exec"`：`exec = {script, workdir?, stdin?, nosandbox?, wait_ms?}`。`wait_ms` 只控制本次前台等待（上限 300s；传输预算 timeout/deadline 至少比它多 5 秒余量）；到点未完成，同一运行被登记为后台任务（不重启、不重放、不换日志），返回 `background=true + id`。
- `action: "fs"`：`fs = {method, args}`，FS 数据面直调（`roots/stat/read/list/write/find/move/copy/mkdir/remove` + 字节源 `source.*`/`upload.*` + AI 文本 `text.*`）。FS 不转成 shell 脚本；规则表门与引擎内建共用同一份策略源。
- `action: "cancel"`：`cancel_id = 待取消的 request_id`。终止同一执行（脚本及受管子进程），与 `bg kill` 共用执行句柄；取消不回滚已发生的副作用，丢失的回复不重放。

RTC 信封额外携带 `ticket`（hello/auth.renew）、`grant_approved`（仅 exec）、`channel`+`stream:{endpoint,args}`（stream.open）。`hello` 响应宣告 `tools_protocol: "hosts_tools/2"`。

## 执行输出（统一形状）

exec 响应 result 恒为 `{content, attrs}`：

- `content`/`attrs.stderr` 的截断只针对 NATS（AI 消费）：content = stdout 前 1000 行，stderr 预览 100 行，任一截断标 `truncated`；RTC 直连（viewer 等非 AI 消费）全量返回 content+attrs——不截断、不转后台、没有 truncated 标记；更多数据（完整日志）经 fs 调用读取。`attrs.output` / `attrs.error_output` = `.exec/{short}.stdout.log` / `.stderr.log` 双流全量日志路径（日志是普通文件，读写由 FS rules 决定）；FS 写审计追加进 stderr 日志；日志创建失败是明确错误，不静默降级。
- 完成时 `attrs.exit_code` 逐码透出（126 权限拒绝 / 127 未找到 / 130 取消 / 124 墙钟到期）；输出截断标 `truncated`，stderr 预览 100 行入 `attrs.stderr`（NATS）。
- 转后台时 `attrs.background=true` + `attrs.id`（任务句柄）。任务归属 = (user_id, session_id)，user_id 取调用方身份（NATS 信封 caller / RTC 票据身份）：list/wait/kill/cancel 全部按归属检查，不匹配等同不存在。

后台任务唯一来源是 exec 前台等待超时，且只服务 NATS/AI 通道（RTC 直连等待超时返回 `deadline_exceeded`，执行继续，可经 cancel 终止，不产生 bg 记录）；任务墙钟 30 分钟（到期 124）。`bg list [--json]` / `bg wait <id> [秒] [--json]`（有界等待，共享本次 exec 前台预算，禁止等待自身）/ `bg kill <id>`。容量闸：全局 max(2, CPU/2) + 单归属 4 个同时运行，容量不足时取消本次执行并返回 `overloaded` 错误（容量检查前先惰性结算已完成任务，不会出现「无法 bg list 解锁」的死锁）。错误与部分结果并存：等待超时（deadline_exceeded）、容量取消等失败响应仍携带 Result（含 attrs.output/error_output 日志地址），Reply 不因 err 非空丢弃 Result。

browser/cua 指令的 `--json` 契约：stdout 只放约定 JSON（--json 紧凑、否则缩进），诊断一律写 stderr；与 shell 自由组合（`browser page.list --json | jq ...`）。activate、`--delivery foreground` 不需要审批——是否允许由 rules 决定（host 装配侧按 exec 域规则门控 browser/cua 指令）。

## RTC stream

stream 仅向前端提供双向异步原始消息通道，不进入 exec 后台执行。`page.frames` 与 `page.input` 是固定的私有端点表（不是指令、不进 commands/caps）；打开时由 Browser 服务按票据身份与页面归属判定。

控制 DataChannel 是 `hosts-tools`；前端创建 `hosts-stream/<id>` 并用一次 stream.open 绑定 endpoint/args。之后直接发送二进制消息，无逐包 RPC、响应、ACK 或应用层 credit。send 仅等待本地缓冲容量，recv 仅等待本地入队消息。关闭 DataChannel 结束转发。

每包最多 32 KiB；接收队列最多 128 包/4 MiB，发送缓冲目标 256 KiB。过载关闭该流；其他流和调用不受影响。NATS/HTTP proxy 不提供 stream。

前端遇到 RTC `disconnected` 保留现有调用和流最多 10 秒，连接恢复后继续使用；`failed`/`closed` 或超过宽限期才清理。下一次打开会替换连接池中的已关闭连接；不自动重放写操作。

## Browser 运行环境

Browser 使用真实 Chrome 的新版 headless 模式，保持后台无窗口运行。每次启动时先用临时 profile 的内置页读取实际 UA、完整 Client Hints 和窗口边框尺寸，再启动长期使用的专用 profile。探测不访问外部网站，也不会读取用户的个人 Chrome profile。

- 将实际 UA 中的 `HeadlessChrome` 归一为 `Chrome`，在进程启动时设置，覆盖 CDP target 创建之前发出的请求；通过原生 Blink 参数关闭 `AutomationControlled` 标记。
- 保留原生低熵 Client Hints，并通过 CDP 向页面、跨进程 iframe、普通/嵌套/共享/Service Worker 设置完整 metadata。版本号、品牌顺序、CPU 架构、位数和系统版本来自同一次真实探测，不硬编码或随机拼接。
- 子目标在恢复执行前完成初始化；Service Worker 的命令按发送顺序排队，再恢复浏览器端启动，避免等待尚未创建的 renderer 导致死锁。不注入网页脚本，不改写 navigator 原型。
- 每个主动创建的页面使用独立隐藏窗口；窗口外框按 Chrome 实测边框尺寸调整，虚拟屏幕至少容纳整个窗口，截图仍按配置视口输出。语言、时区、字体和 GPU 保持 Chrome 原生行为。
- 专用 profile 持续复用。退出先发送 `Browser.close`，给 Cookie、localStorage 等数据正常落盘的机会；无响应时才强制回收专用进程。

这是浏览器环境一致性处理，不承诺无法被网站识别为自动化。首次打开页面增加一次短暂的 Chrome 本地探测；已经运行的实例不重复探测。实现依据：[Chrome Headless](https://developer.chrome.com/docs/automation-and-testing/headless)、[CDP](https://chromedevtools.github.io/devtools-protocol/)、[Chromium UA/Client Hints](https://chromium.googlesource.com/chromium/src/+/main/components/embedder_support/user_agent_utils.cc)。

上传先复制到专用暂存区，Chrome 的 File 对象可能在调用返回后才读取磁盘，因此暂存保留到所属页面关闭（服务退出也清理；下次启动清理崩溃残留）。默认单文件 256 MiB、最多 128 份、总计 1 GiB；达到上限需关闭持有上传的页面释放空间。导航或重复设置 input 不提前删除，以免破坏页面仍持有的 File 对象。`page.wait hidden` 仅在匹配数为零或唯一匹配已隐藏时成功，多个匹配继续等待。

下载无墙钟过期：记录保留到所属页面关闭（进行中走 CDP 取消，文件与记录一并回收；服务退出同样清理）。容量淘汰只在新下载登记时触发——记录数达 128 或总字节预留不出一份单文件上限（默认 256 MiB，总量默认 1 GiB）时，按创建先后淘汰最旧的非进行中下载；进行中下载永不被动淘汰，进行中超单文件/总量上限即取消。

CDP 待派发事件最多 512 条/32 MiB。积压时丢弃旧 screencast 帧、Network 遥测和 Runtime 日志，生命周期事件保持顺序；帧 ACK 独立于消费者发送，丢帧仍 ACK。全为关键事件的溢出或 ACK 队列无法推进仍明确断开，避免无限内存或静默丢失页面状态。

CUA 写管道不持状态锁，取消或关闭会中断堵塞写入；部分消息写入后销毁该驱动连接，不重放交互。NATS 每条连接只持有一个心跳，连接替换前等待旧心跳退出。

## FS 与文件代理

前端 RTC 与 HTTP 文件代理都调用同一个 FS；代理经服务器签名转换成 hosts_nats/2（action=fs），保留真实用户身份和 fs-only 授权约束（不能借文件入口执行命令）。没有随机 AI session、通用 operation/resource 注册表或代理专用协议。

FS 源的版本、校验和、暂存归 FS 所有。读写通过 24 KiB 有界范围调用：source.read、upload.open/write/seal。范围写入只允许顺序写或相同偏移相同内容重试；seal 校验尺寸和哈希。fs.write 使用 absent/version/any 条件提交。网络丢回复可以恢复只读调用与幂等范围，不能重放文件提交、移动或删除。

默认单源上限 512 MiB、proxy 上传上限 64 MiB、总字节存储 2 GiB、最多 128 个源；源 30 分钟过期。配置为 hosts_upload_bytes、hosts_proxy_upload_bytes、hosts_sources，0 使用默认值。降低配额影响后续创建，不改写已分配对象。配置工作目录只决定 home，不限制可访问根；访问始终检查文件策略。

FS 写授权按「谁被创建/修改就查谁」判定：write/edit/remove 只要求目标自身；mkdir -p 与 write/curl -o 等补父目录的便利逻辑只对实际创建的层级做写检查（先检后建、拒绝时零副作用），已存在的祖先层级不要求写授权。存在性探测本身不做策略门控——目标路径祖先链的存在性因此对调用方可观察，但不暴露目录内容。

原生 hostfs 支持 macOS、Linux 和 Windows，AI 文本工具与 RTC/代理共用实现。Windows 读取通过父目录句柄打开单个叶节点并拒绝 reparse point；移动用原生相对句柄重命名，absent 条件原子禁止覆盖。写入在关闭写句柄后提交并返回稳定版本；条件新建不要求文件系统支持硬链接。

## 验证和边界

Go race 覆盖设备分发、授权、统一执行外层（前台/转后台/容量/取消）、FS 和真实 RTC。服务器测试覆盖真实 NATS 签名/权限（含 grant_approved 篡改验签失败）以及 HTTP→NATS→设备 FS→Node SDK：二进制、中文/BOM/CRLF、空文件、分页、范围恢复、条件冲突、写入回复丢失。

macOS Chrome 已实测自动输入、边缘滚动、观察、下载/上传、弹窗、popup。前端覆盖文件路由、缓存、浏览器输入与解码、设备选择及空白页。桌面 Chrome 的四目标归档已校验哈希与资源布局，macOS arm64 已验证实际打包、签名与包内 Chrome。Windows/Linux 仅完成交叉编译和归档检查，仍需原生环境运行验收；可选 script 编排尚未接入。
