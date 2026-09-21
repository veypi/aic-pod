# 设备能力统一协议与实现

更新：2026-09-21。本次归一化已落地，详细职责图见 [统一设计](hosts-protocols-proposal.md)。平台、设备和前端需要共同升级，没有旧协议兼容入口。

## 能力和注册

AI 只有内建 `fs` 和 `exec`。Browser、CUA、shell、git、curl、json 和 `bg_*` 全部是 `exec.commands`；不存在第三份 `caps.tools`。

- `protocol/hosts_tools` 定义本地声明、call/stream、统一调用体和错误。
- `libs/hosts_tool` 用 `RegisterCommand` / `Bind[A,R]` / `BindStream[A]` 一次注册，生成 schema、argv 翻译、帮助及权限描述。原始程序保留 `argv`，服务方法使用 typed 参数。
- `libs/hostauth` 负责 RTC 票据、DTLS 身份绑定和续期；NATS 验签在 `libs/host/tools.go`。
- `libs/exec_procs` 维护一次执行、取消、独立输出和最终结果。进程、函数和服务方法共用。
- `libs/hostfs` 拥有路径、版本、上传和字节源；AI 文本操作与前端二进制文件操作使用同一 FS 实现。
- `libs/browser` 拥有 Chrome、页面、refs、下载和输入状态；`libs/cua` 拥有驱动、窗口和快照。`ui/1` 是能力内部语义，不是宿主协议。

方法目录由同一注册表生成。`catalog` 默认返回轻量索引；`catalog: {domain:"exec",command:"browser"}` 或 `{domain:"fs"}` 返回对应完整 schema，避免整个目录超过 RTC 消息上限。两者都返回 `exec.epoch`。NATS 和 AI 目录过滤 stream；RTC 目录允许 stream。

## 调用

RTC 完成 `hello` 后使用 `hosts_rtc/1`。服务器使用签名的 `hosts_nats/1`，封装同一 Request。

```json
{
  "protocol": "hosts_rtc/1",
  "request_id": "r_example",
  "action": "call",
  "timeout_ms": 30000,
  "call": {
    "domain": "exec",
    "command": "browser",
    "method": "page.create",
    "args": {"url": "https://example.com"}
  }
}
```

内建文件调用使用 `domain:"fs"`，不带 command；例如 `method:"stat", args:{path:{root_id:"root",segments:["tmp","a.txt"]}}`。真实根 ID 来自 FS roots。AI 的 fs.read/edit/rg 等翻译成同一 FS 的 `text.*` 方法。

`argv:["browser","open","https://example.com"]` 与 call 二选一。服务方法由声明翻译参数，原始程序使用 `{domain:"exec",command:"sh",method:"run",args:{argv:["-c","echo hello"]}}`。位置参数、别名、权限提升均来自声明。`commands` 返回同一目录生成的 AI 帮助。

普通 call 默认 30 秒，执行超时上限 30 分钟；AI 工具另外保留各命令的更小超时上限。request_id 关联当前等待，`call.cancel` 携带 cancel_id。取消不承诺动作回滚。活动调用按主体、AI 来源、连接和 ID 隔离；请求或写入的回复丢失不会自动重放。

## 执行、后台与输出

方法用 `Background:true` 声明可继续在后台运行。原始进程、函数命令、Browser 的 page.wait/download.wait、CUA 的 window.wait 使用同一个执行管理器。短查询与控制方法直接调用。

```json
{
  "protocol": "hosts_rtc/1",
  "request_id": "r_wait",
  "action": "call",
  "call": {
    "domain": "exec", "command": "browser", "method": "page.wait",
    "args": {"page_id": "p_example", "text": "完成", "timeout_ms": 60000}
  },
  "timeout_ms": 60000,
  "execution": {
    "epoch": "从目录取得的执行 epoch",
    "id": "e_preallocated",
    "wait_ms": 0,
    "output": "/允许写入的路径/wait.log"
  }
}
```

- `wait_ms` 只控制本次等待，范围 0–300000，默认 30000。0 立即返回执行记录。
- `timeout_ms` 控制此次运行上限；Browser/CUA 方法参数里的等待条件仍可另设更短期限。
- NATS 的 deadline/nonce 控制入场；签名 authorization_until 控制执行授权，最长 30 分钟。RTC 入场后的执行按设备执行策略取得独立预算，断线不续期也不取消合法后台任务。
- `bg_list` 返回本来源的运行中执行；`bg_wait ID [--wait SECONDS]` 读取同一执行；`bg_kill ID` 发起取消。
- 进程取消终止该进程树；服务方法通过 context 取消，保留共享服务和其他页面。handler 实际结束后才标记 cancelled。
- 同一 epoch、主体、来源和 execution.id 去重；不同参数复用 ID 返回 conflict。等待时长可以改变，执行内容、期限和输出不能改变。重启后的旧 epoch、已过期 ID 明确返回 expired。
- 完成响应包含 id、command、status、output、content、truncated、result/error；只有进程才有 exit_code。bg_wait 的 typed result 与直接完成一致。
- 每次执行有独立 Writer，进程 stdout/stderr、服务显式进度和最终可读结果写入该 Writer。结构化结果独立保留。
- 指定 output 需要文件写权限，AI 还需要 fs 工具开启。文件使用排他创建，不覆盖已有文件。默认日志由 exec 分配；显式文件在记录过期后保留。

执行记录完成后保留 1 小时，最多 512 条；每份日志 16 MiB，预览最多 1000 行/128 KiB，运行中的预留加保留输出最多 256 MiB。截断显式标记。当前运行时最多保留 8192 个已使用身份的防重放记录，达到配额拒绝新执行。首版不承诺重启续跑或跨设备迁移执行。

## RTC stream

stream 仅向前端提供双向异步原始消息通道，不进入 exec 后台执行。工具返回 `Send(ctx, []byte)`、`Recv(ctx) ([]byte,error)`、`Close()` 端点，宿主不解释内部事件格式。

控制 DataChannel 是 `hosts-tools`；前端创建 `hosts-stream/<id>` 并用一次 stream.open 绑定 command/method/args。之后直接发送二进制消息，无逐包 RPC、响应、ACK 或应用层 credit。send 仅等待本地缓冲容量，recv 仅等待本地入队消息。关闭 DataChannel 结束转发；流权限持续复查。

每包最多 32 KiB；接收队列最多 128 包/4 MiB，发送缓冲目标 256 KiB。过载关闭该流；其他流和调用不受影响。是否丢弃或合并消息由工具和前端决定。NATS/HTTP proxy 不提供 stream。

Browser 的 `page.frames` 与 `page.input` 分开授权，消息格式由 Browser viewer 私有实现。前后端保留最新待处理移动/滚动，离散按键点击保序；帧接收与解码分离，保留最新待画帧。真实输入自动进入人工控制，空闲 10 秒退出；无接管按钮或接管 call。Chrome 专用实例关闭边界弹性回滚。

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

CDP 待派发事件最多 512 条/32 MiB。积压时丢弃旧 screencast 帧、Network 遥测和 Runtime 日志，生命周期事件保持顺序；帧 ACK 独立于消费者发送，丢帧仍 ACK。全为关键事件的溢出或 ACK 队列无法推进仍明确断开，避免无限内存或静默丢失页面状态。

CUA 写管道不持状态锁，取消或关闭会中断堵塞写入；部分消息写入后销毁该驱动连接，不重放交互。NATS 每条连接只持有一个心跳，连接替换前等待旧心跳退出。

## FS 与文件代理

前端 RTC 与 HTTP 文件代理都调用同一个 FS；代理经服务器签名转换成 hosts_nats/1，保留真实用户身份和 fs-only 授权约束。没有随机 AI session、通用 operation/resource 注册表或代理专用协议。

FS 源的 epoch/ID、版本、校验和、暂存归 FS 所有。读写通过 24 KiB 有界范围 call：source.read、upload.open/write/seal。范围写入只允许顺序写或相同偏移相同内容重试；seal 校验尺寸和哈希。fs.write 使用 absent/version/any 条件提交。网络丢回复可以恢复只读调用与幂等范围，不能重放文件提交、移动或删除。

默认单源上限 512 MiB、proxy 上传上限 64 MiB、总字节存储 2 GiB、最多 128 个源；源 30 分钟过期。配置为 hosts_upload_bytes、hosts_proxy_upload_bytes、hosts_sources，0 使用默认值。降低配额影响后续创建，不改写已分配对象。配置工作目录只决定 home，不限制可访问根；访问始终检查文件策略。

FS 写授权按「谁被创建/修改就查谁」判定：write/edit/remove 只要求目标自身；mkdir -p 与 write/curl -o 等补父目录的便利逻辑只对实际创建的层级做写检查（先检后建、拒绝时零副作用），已存在的祖先层级不要求写授权。存在性探测本身不做策略门控——目标路径祖先链的存在性因此对调用方可观察，但不暴露目录内容。

原生 hostfs 支持 macOS、Linux 和 Windows，AI 文本工具与 RTC/代理共用实现。Windows 读取通过父目录句柄打开单个叶节点并拒绝 reparse point；移动用原生相对句柄重命名，absent 条件原子禁止覆盖。写入在关闭写句柄后提交并返回稳定版本；条件新建不要求文件系统支持硬链接。

## 验证和边界

Go race 覆盖设备分发、授权、统一执行、FS 和真实 RTC。服务器测试覆盖真实 NATS 签名/权限以及 HTTP→NATS→设备 FS→Node SDK：二进制、中文/BOM/CRLF、空文件、分页、范围恢复、条件冲突、写入回复丢失。

macOS Chrome 已实测自动输入、边缘滚动、观察、下载/上传、弹窗、popup；统一 exec 的真实 Chrome wait 取消后页面仍存活。前端覆盖文件路由、缓存、浏览器输入与解码、设备选择及空白页。桌面 Chrome 的四目标归档已校验哈希与资源布局，macOS arm64 已验证实际打包、签名与包内 Chrome。Windows/Linux 仅完成交叉编译和归档检查，仍需原生环境运行验收；可选 script 编排尚未接入。
