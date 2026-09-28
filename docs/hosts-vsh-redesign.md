# vsh、权限与宿主协议：简化设计

状态：评审修订稿，2026-09-28；尚未实施，暂缓代码迁移。不考虑旧等级、旧载荷的兼容。本文是本轮目标契约，design.md 和 host_sandbox.md 同步描述目标，不把它们视为当前实现完成证明。

核心原则：**vsh 是面向 Agent 的 Bash，执行单位是完整脚本。bg、cua、browser 与其他 shell 指令一样，可以自由组合；审批在发送前完成，执行端只执行并按 rules 检查权限。** 保留 vsh、规则和沙箱实现，收敛脚本入口、前后台转换和统一输出，不增加新的执行框架。

## 1. 各部分只做自己的事

| 部分 | 职责 |
|---|---|
| aic 工具层 | 工具开关、目标检查、必要的用户审批；通过后发送请求 |
| NATS / RTC | 身份认证、请求传输和响应关联 |
| exec 接入层 | 前台等待、超时转后台（仅 NATS/AI）；统一保存脚本输出日志、生成返回预览和 attrs（预览截断仅 NATS；RTC 全量返回）——编排机制共享自 libs/execwait |
| vsh | 业务无关的 bash：可注册指令，执行 script，返回 stdout/stderr/exit_code；不管日志文件、预览截断、attrs 或转后台决策 |
| bg | 查询、等待、取消已转入后台的执行，不负责启动或保存输出 |
| rules 与沙箱 | 判断实际资源访问是否允许，拒绝时返回权限错误 |
| grant | 修改规则前检查可信 grant_approved；通过后更新 rules |
| Browser / CUA / FS | 实现各自业务，不再声明另一套工具方法和权限等级 |

认证通过的 NATS 请求就是服务端已经允许执行的请求。pod 不重新分类审批、不等待人工确认，也不把权限错误转成审批。允许执行脚本不等于允许访问所有资源或允许其中的 grant 修改授权；后者还须携带服务端确认过的 grant_approved 事实。

## 2. vsh：保留 shell，只精简发现

### 2.1 指令解析

沿用 vsh 自身的命令注册机制，`commands / grant / bg / browser / cua` 都直接注册为 vsh 指令，不另建宿主 Registry。

vsh 按 shell 语义执行整个脚本；执行到其中一条指令时，按以下顺序查找实现：

1. 优先使用 vsh 已注册的虚拟指令。
2. 未命中时，由宿主原生适配器按 PATH 查找并执行；显式程序路径也由该适配器处理。
3. 都没有则返回 command not found。

原生程序不逐个注册；`git status` 就是 `git status`，不要求写成 `native git status`。已有 NativeFallback 方向保留，只去掉用二进制注册表重复维护命令授权的做法。

命中指令但被 rules 拒绝时，直接返回权限错误，不能继续 fallback 绕过拒绝。原生 fallback 同样受命令规则和进程沙箱约束；它不是一条免检查通道。

原生能力由 aic-pod 的宿主适配提供，不让 vsh 核心默认直接调用宿主 OS。page 没有原生执行能力，只运行已注册的虚拟指令。

### 2.2 commands 只展示需要发现的能力

- host/cloud：默认只展示核心自定义指令，例如 `commands / grant / bg / browser / cua`，以及该端实际提供的 `list_hosts / send_user`。
- `ls / rm / mkdir / cat` 等常见指令不占发现目录；不枚举 PATH，不展示本机全部二进制。
- page：展示该端全部已注册指令，因为没有本机命令环境可供假定。
- 具体用法通过 `<command> --help` 获取。

这是**展示过滤**，不是执行白名单，也不要求删除 vsh 已有的常用指令实现。未列出的指令照常可执行。

`commands` 直接读取 vsh 的注册信息，host/cloud 仅筛选核心自定义名字；不增加命令描述 DSL、方法表或权限元数据。命令发现也通过脚本执行 `commands` 和 `<command> --help` 完成，不另设 catalog RPC。

### 2.3 执行的是完整脚本

所有执行请求只走 `Engine.Exec(script, options)`。`bg / cua / browser` 没有特殊执行模式，可以与 Linux 指令、管道、重定向、变量、条件和循环混用：

```sh
browser page.list --json | jq '.'
cua --help | grep usage
bg list | grep running
git status && browser page.list --json
```

vsh 负责整段脚本的解析、展开和控制流。各指令使用普通 stdin/stdout/stderr 和退出码参与组合，管道连接指令输出，重定向作用于相应文件；协议层不逐条拆分执行，不按指令名选择返回格式。

前端、Agent、NATS 和 RTC 全部提交脚本。命令注册和参数解析只是 vsh 内部的实现细节，不形成外部调用接口。

### 2.4 先前台执行，等待超时才登记后台任务

每次 exec 开始时直接运行脚本，只持有本次执行需要的上下文、完成通知和日志文件；**不创建 bg 记录，不分配 bg ID**。request_id 用于请求关联和日志命名，不是后台任务 ID。

- 在前台等待期限内完成：返回结果，结束本次执行，不留下 bg 记录。
- 等待到期仍未完成：将这次仍在运行的执行登记到 bg 表，此时才分配 ID；返回 `attrs.background=true` 和 `attrs.id`。
- 转后台只是交接已有执行的管理权：不重新启动脚本，不重放前序操作，不更换日志文件。完成与超时的竞争只产生一个结果，不能重复登记。

转后台（bg）只服务 NATS/AI 通道——后台任务是给模型后续查询/等待/取消用的句柄。RTC 直连（viewer 等人工调用）等待到期不登记 bg：返回 `deadline_exceeded`，执行继续，调用方可经 `cancel(request_id)` 终止，断连清理只影响这类前台执行。page 无长时间任务，不接 bg。

前台等待期限和执行本身的最长运行期限分开；转后台不重置运行期限。日志由实际执行结束时关闭，不随前台回复结束而关闭。

bg 只保留 `list / wait / kill`：查询状态、等待完成、取消执行。删除 `bg run` 和 `bg output`；后台任务唯一来源是 exec 等待超时。脚本自身的 `&` 仍是 shell 内部并发，不逐条注册成 bg 任务。

bg 表只保存后台执行的 ID、归属、状态、退出码、日志路径和取消/完成句柄，不另存一份输出缓冲。沿用完成记录的有界清理，不做持久任务恢复。若转后台时容量不足，取消这次执行并返回容量错误与日志路径，不让未登记的执行继续运行。

归属固定为 `(user_id, session_id)`，不包含传输类型或 agent_id。同一用户、同一会话里的 AI/NATS 和用户/RTC 可互相查询、等待、取消任务；跨会话默认不可见。RTC 的 session_id 必须由票据绑定，不能由脚本或未经核验的请求字段冒认；无会话的手动调用属于独立的 manual 范围，不隐式获得所有 AI 会话任务。cancel 和 bg 使用相同归属检查。日志是普通文件：读写完全由 FS rules 决定，按会话分目录只是整理文件，不构成权限边界；跨会话读取只要 rules 允许就是正常行为，不为日志增加 ACL 或隐藏权限层。

执行 `bg list / wait / kill` 本身仍是普通 exec，不预占 bg 名额。后台任务满额或另一个脚本正在运行，不能阻塞查询和取消；该要求也适用于 `bg list | grep running` 这样的组合脚本，不靠单指令特判。

`bg wait <id> [秒]` 是有界等待：实际等待不超过指定秒数与本次 exec 剩余前台预算减 1 秒的较小值；未指定秒数时使用后者，预算不足时立即查询。到点返回目标当前状态，不自动重试、不独立登记等待任务；同一脚本多次 wait 共用剩余预算，不能每次重新获得等待额度。后台执行里的 wait 只查询状态，不再阻塞；禁止等待自身。普通 wait 因而不会派生等待链；调用者显式循环或脚本其他耗时工作仍适用整段脚本的普通超时规则。

### 2.5 所有 exec 统一记录输出

每次 exec 由接入层在运行脚本前创建两个文件：`<request_id>.stdout.log` 和 `<request_id>.stderr.log`，经引擎的 stdio 接线分别持续写入脚本最终的 stdout 和 stderr（引擎只写，不创建、不命名、不关闭）。普通完成、执行失败和超时转后台使用同一机制；不再将两流混成一个文件，也不引入分段日志格式或要求 viewer 过滤诊断文本。

- `attrs.output` 为 stdout 文件地址，`attrs.error_output` 为 stderr 文件地址，使用对应 host/cloud/page 的可读取路径；文件创建成功后的失败响应也保留已有地址。
- 截断只针对 NATS（AI 消费）：content 只包含 stdout 预览（前 1000 行），`attrs.stderr` 只包含 stderr 预览（前 100 行），任一超限设置 `attrs.truncated=true`。RTC 直连（viewer 等非 AI 消费）全量返回 content+attrs——不截断、不转后台、没有 truncated 标记；更多数据（完整日志）经 fs 调用读取。不能由 browser/cua/bg 或其他指令自行决定 exec 的响应截断。
- 截断只影响返回预览，不截断日志，也不截断管道中间数据。不先在 vsh 内把输出丢掉再写日志；不为了预览在内存里保存全量输出。FS 写审计等诊断信息追加进 stderr 日志，不污染 stdout 日志的 --json/管道契约。
- 转后台后继续写原来的两个文件。需要后续输出时，通过现有 FS 或普通 `cat / tail` 读取相应路径，不再提供 bg 专用输出接口。

内部命令只使用正常标准流，不自行建立执行日志，也不为适配 exec 手动插入重定向。用户脚本显式写的管道和 `>` 仍按正常 shell 语义执行：自动日志记录最终流出脚本的 stdout/stderr，不旁路复制管道中间内容或用户重定向文件。

`bg list/wait` 以普通输出报告目标任务的状态和两个日志路径，不回放目标任务日志；本次查询 exec 的 attrs 仍指向查询本身的日志。日志创建或写入失败要明确报错，不能声称输出已经完整保存。

### 2.6 等待、传输与取消

前台等待 W 小于传输超时 T；NATS request-reply、RTC 及外层 HTTP 等待都按 `T >= W + 5 秒` 配置，余量用于传输和回包。W 经执行端上限钳制后的有效值必须由双方一致使用，排队与处理开销计入传输预算。授权有效期和整次运行期限另行限制执行，不因重连或转后台延长。

传输超时、断线与前台等待结束都不等于显式取消。调用方 ctx 结束只结束前台等待：不取消执行、不登记 bg——执行继续跑完写日志，取消唯一来源是 cancel/bg kill（句柄）或墙钟到期。即使保留余量，回复仍可能丢失：调用方报告结果未知，使用已有请求关联/去重信息、bg 列表和日志核实，不自动换 request_id 重跑脚本。nonce 防重放不等于业务 exactly-once。

`cancel` 按原 request_id 取消实际执行，`bg kill` 按后台 ID 找到同一个取消句柄；两者都终止脚本及受管子进程，不是只放弃等待。前台转后台的竞争不丢失 request_id 的取消关联。取消不回滚已发生的副作用，待实际执行结束后才报告终态。单纯不想继续等待，调用方停止等待即可，不发送 cancel。

## 3. 审批与权限正交

### 3.1 工具配置与审批

删除数字等级。工具配置只表达是否启用和允许的目标；是否需要审批是服务端对本次请求的判断，不是工具权限档位。

触发审批的条件只有两个：修改授权的 `grant` 和显式 `nosandbox`。审批单位是一个 Tool.Message，沿用它已有的状态机；任一条件命中都对完整请求审批一次，同时命中也不拆成两次审批或两份审批状态。通过即允许本次脚本中的 grant 修改，以及请求显式指定的 nosandbox。`grant status`、help 不触发审批。Browser/CUA 不需要审批，CUA 的 `activate`、`--delivery foreground` 也不例外；这些操作是否允许由 rules 决定。

审批分类是服务端调用的小函数，复用 CLI 参数解析，不放到命令目录，不恢复 AccessRules / RequiredLevel。请求未获允许就不发送；删除通用 confirmed/Level=9，只携带 `grant_approved` 布尔事实，默认 false。它由 Tool.Message 的整单审批结果导出，供 grant 修改入口校验，不是单独的 grant 审批状态。

静态分析用于提前提示审批，不能作为授权修改的安全边界。真正的边界是 grant 修改入口：每次实际写规则前读取可信上下文，`grant_approved=false` 就返回 permission_denied，规则不变，不暂停、不转 waiting、不重放脚本。该检查覆盖变量拼接、命令替换、eval/source 和嵌套脚本；status/help 不要求此标记。所有规则修改分支使用同一入口，不能另留不检查的路径。

AI/NATS 路径的 grant_approved 只由服务端在用户明确批准这条 Tool.Message 后置为 true，无论该次审批由 grant 还是 nosandbox 触发；无需人工审批而直接允许执行的普通请求仍为 false。审批绑定目标、完整脚本、cwd 和执行选项；正文变更重新确认。标记通过现有签名信封传递并转入不可由脚本修改的 context，不进入模型工具参数、argv、env 或会话配置。同一次执行的 eval/source、管道和超时转后台继承该事实，后续独立 exec 不继承。

这是整段脚本和执行选项的整单批准，不声称只批准静态识别出的某一路径。审批界面明确展示这一范围，不提供 grant/nosandbox 分项审批。通过后两者的审批要求都已满足，但批准不会隐式修改请求：未指定 nosandbox 仍使用沙箱，rules 只有在 grant 实际执行时才修改。标记不绕过 grant 本身的命令准入和领域边界。

RTC 是已认证 owner 的直接操作入口：前端对本次完整请求确认一次，统一覆盖 grant 和显式 nosandbox，不分别确认；RTC 适配器将该确认元信息转成当前请求的上下文，默认不批准，不因连接已认证而一概设为 true。该入口信任 owner 前端，不宣称能够证明物理点击；agent 不能使用它，必须走服务端审批路径。Browser/CUA 不增加审批。

### 3.2 执行权限只看 rules

沿用现有 exec/fs/net/ssh 规则及匹配实现，不因协议改造重写配置、默认值或匹配顺序。虚拟命令和原生适配器检查执行规则；FS、网络等实际访问点检查对应资源规则；原生进程由同一规则派生的 OS 沙箱约束。

审批通过不会改 rules，也不会自动免沙箱。权限不足就返回清晰错误，例如：

```text
permission_denied: fs write /work/output 被规则拒绝；可用 grant fs /work/output 申请权限
```

正常流程：原操作返回权限错误 → AI 提交包含 grant 的新脚本 → 服务端审批 → pod 执行脚本。grant 可以单独使用，也可以通过 `&&` 等与后续操作组合；它没有独立于脚本的调用协议。如何继续由 AI 明确提交，执行端不自动申请、不自动重跑已部分执行的原脚本。

`grant cmd git` 只修改命令规则，不再把 git 注册成虚拟指令。会话授权、`--permanent` 及各端原有的授权边界继续由 grant 实现处理。grant 本身不是绕过目标归属或云端用户根的通道。

身份和会话来自已认证请求，通过执行上下文传递；不从脚本可修改的 `AIC_VSH_LEVEL / SESSION / NOSANDBOX` 环境变量取授权信息。不为此新建一套 Authority 框架。

### 3.3 沙箱与 GUI 的边界

原生执行默认使用按 rules 生成的沙箱；无法落实隔离时报错，不静默裸跑。`nosandbox` 是本次执行的显式选项，在发送前审批；它不是“已审批”的同义词，也不引入新的全局配置开关。

免沙箱后不能声称原生子进程仍受 fs/net 隔离；进程内 FS 服务照常检查 rules。Browser/CUA 沿用自己的服务边界，文件规则不冒充对其他 GUI 应用所有行为的限制。本次不增加应用白名单、站点 ACL 或操作风险框架，也不擅自修改这些能力的开关默认值。

## 4. 协议：改载荷，不造新框架

保留 `hosts_tools / hosts_nats / hosts_rtc` 分工，修改后使用新主版本，不做兼容分支。不新增 `hosts` 协议族。

### 4.1 hosts_tools：脚本请求

hosts_tools 只定义传输无关的数据结构，不注册业务命令。请求包含 `request_id`、`action` 及对应载荷。

执行请求只传完整脚本，例如：

```json
{
  "request_id": "r1",
  "action": "exec",
  "script": "browser page.list --json | jq '.'"
}
```

协议不区分脚本里用了哪些指令，也不要求脚本只有一条指令。

保留工作目录、stdin、等待时限和 nosandbox 选项；exec 外层按第 2.4、2.5 节处理前台等待和日志，脚本交给 Engine.Exec。不再包装成 `exec.run` 方法。

另外只保留非命令执行用途：

- `fs`：载荷为 `fs: {method, args}`，直接交给现有 FS 服务。
- `cancel`：按第 2.6 节取消实际执行，与 bg kill 共用取消机制。

每个 action 只接受对应载荷。删除命令 call 和 catalog 载荷，不再暴露 Method/Input/Access/Translate。响应仍为 `request_id + result/error`；exec 结果是整段脚本的输出预览和 attrs，不是逐指令的业务对象。JSON 也只是输出文本，可以经过管道过滤或重定向。

所有已创建日志的 exec 都返回 attrs.output 和 attrs.error_output；content 为 stdout 预览，attrs.stderr 为 stderr 预览；完成时包含 exit_code，任一预览超限时包含 truncated；只有等待超时转后台时才包含 background 和 id。请求在日志创建前即失败时明确报错，不伪造文件地址。沿用 attrs 的字符串值约定，不为日志新增结果协议。

### 4.2 hosts_nats：可信转发

保留现有签名、目标、调用者、会话、scope、nonce 和有效期机制；删除 granted_level，信封加入默认 false 的 grant_approved，载荷改成上述请求。现有 HMAC 必须覆盖该标记和完整请求；模型不能自行设置，篡改标记必须验签失败。

服务端在发送前完成工具开关、目标和审批检查。pod 验证信封后分发执行，不再运行审批分类函数，不返回 waiting/approval_required。文件代理原有 fs-only 范围继续有效，不能借文件入口执行命令。

grant_approved 是既有可信请求上下文中的审批结果，不是新签发、可独立流转的审批票据。pod 不重新决定要不要审批，只在 grant 修改入口检查是否已获准。本次不增加独立审批凭据、第二次签名、策略版本协商或新的执行 ID/epoch 机制；已有身份验证、去重与请求关联照常保留。

### 4.3 hosts_rtc：相同脚本请求，私有 stream

保留现有连接认证、续期、撤销和数据通道机制；普通请求使用与 NATS 相同的载荷和分发实现。

`page.frames / page.input` 留在 RTC 私有端点表，直接连接 Browser 服务；不注册为 vsh 指令，不进入 commands/caps。该表只管理 RTC 流，不变成通用工具注册表。

stream 继续检查调用者、页面归属、对应能力规则、租期和人工控制状态。连接失效关闭流；不因 stream 移出命令目录而取消这些检查。

### 4.4 FS 与前端

FS 是数据面，保留现有路径、版本、上传、字节源和条件提交语义；不转成 shell 脚本。FS RPC 与 vsh 文件操作共用同一份资源规则。

前端 Browser/CUA viewer 从 `method + args` 改为提交普通 vsh 脚本，可以包含管道和其他指令。所有动态参数统一使用拟新增的 `aic/ui/assets/libs/shell_quote.js` 中 `shellQuote(value)`，由共享前端基础库维护者负责审计；所有 viewer 引用这一实现，禁止各自转义或直接插值动态值。固定命令名、flag 和 shell 运算符是代码中的已审查文本，不来自页面数据。

shellQuote 使用 vsh 支持的单引号字面量规则，处理内嵌单引号与空串，保留空格、换行和 Unicode，拒绝 NUL。以 `-` 开头的值另外按 CLI 的带值 flag 或 `--` 边界传入，不能把 shell 转义误当成选项注入防护。配套 `shell_quote.test.js` 覆盖引号、反斜杠、美元符、反引号、换行、通配符、分号及命令替换，并在 vsh 中断言还原值一致且没有额外指令被执行。这只是文本转义函数，不恢复 argv 协议或方法翻译表。

### 4.5 viewer 消费的 CLI 输出契约

viewer 使用的 Browser/CUA 指令和 bg 查询提供 `--json`：stdout 只输出约定 JSON，提示、诊断和进度写 stderr。成功 JSON 的字段、类型和空列表形状固定；同一主版本只能做兼容新增，不能随人类可读输出调整。非零退出不能当成功数据使用；未知字段可忽略，必需字段缺失或类型变化显式报错。

- `browser page.list --json` 输出 `PageInfo[]`，沿用 `libs/browser/types.go` 的字段定义。
- CUA 的 app/window 列表及其他 viewer 操作沿用 `protocol/ui` 的 Result 结构；实施时补齐每个操作的结果 JSON Schema，data 内被 viewer 使用的字段也要显式定义和用固定样例锁定，不能只依赖 Go 的 any 或输入 schema。
- `bg list --json` 输出任务数组，固定字段为 id、state、script、output、error_output；已完成时有 exit_code。`bg wait <id> --json` 输出同形单个任务对象，运行中不伪造退出码。state 固定为 running/done/timeout/killed/error。

JSON schema、黄金样例和命令输出测试由各命令所属包维护；实施时同步增加 Browser/CUA/bg 的 CLI 样例，并在前端做消费契约测试。它们约束 stdout 内容，不进入命令目录或协议分发，不是第二套单指令调用机制。

viewer 只在整段脚本完成且成功后解析 stdout；若 attrs.truncated=true，则读取 attrs.output 的完整内容再解析，绝不从 stderr 或混合日志中过滤出 JSON。后台执行先查询完成状态再读文件，不能解析仍在增长的半截 JSON。脚本组合后的最终格式由提交方负责；用户主动写 `2>&1` 或拼出多个 JSON 文档，不是执行端应修复的情况。删除 tool_utils 的旧方法翻译及兼容分支。

## 5. 只做必要改动

1. 删除数字等级及环境变量授权，审批留在发送端；添加可信 grant_approved 及 grant 修改入口检查，先堵住动态扩权通道，权限继续沿用 rules。
2. Browser/CUA 直接注册为 vsh 指令；所有命令执行沿用 Engine.Exec；保留 PATH fallback，去掉原生二进制逐个注册；精简 commands 展示。
3. exec 改为先前台运行、超时才登记 bg；统一双流日志、预览截断和 attrs，落实传输余量、cancel、bg wait 预算和归属；bg 只保留 list/wait/kill。
4. hosts_tools、NATS/RTC 和前端统一提交脚本；完成共享 shellQuote、--json 契约和 viewer 日志读取；删除独立命令 call/catalog；FS 保持独立，stream 留在 RTC。
5. 删除 hosts_tool 声明体系、Method/Translate/AccessRules/RequiredLevel、DeviceCommand 投影和旧协议兼容代码。

验收只围绕这些变化：

- bg/cua/browser 与 Linux 指令混合使用，管道、重定向、条件和循环遵守相同 shell 语义。
- 未超时的 exec 不产生 bg 记录；超时只登记原执行一次，不重跑；bg wait 在共享前台预算内返回状态，不派生等待链；后台满额时仍可查询和取消。
- 每次 exec 自动分流保存 stdout/stderr 并返回两个路径；响应截断不丢日志、不改变管道数据；stderr 诊断不污染 --json；转后台继续使用原文件。
- 只有 grant 授权修改和 nosandbox 触发审批；一个 Tool.Message 只做一次整单审批，通过即满足本次请求两者的审批要求。无 grant_approved 的字面/动态/eval/source grant 均不修改规则；env 伪造无效、签名篡改失败；未指定 nosandbox 不因批准而关闭沙箱。
- 传输超时大于前台等待及余量；丢失回复不自动重跑；cancel 和 bg kill 都取消同一实际执行，转后台竞争不丢取消句柄。
- 同 user/session 的 NATS/RTC 互见互管，跨会话及伪造 RTC session 被拒；任务和日志采用一致归属。
- 前端动态参数转义不改变参数含义或引入额外命令；虚拟指令优先，未命中可走 PATH；规则拒绝不能被 fallback 绕过。
- 普通系统指令不挤占发现表而 page 列全；不同传输访问同一资源遵守相同 rules。

文档与工具说明同步：aic-pod 的 design.md、host_sandbox.md 更新为本目标的概览；aic 的 instruction_sets_v2.md 标明旧契约已被本提案替代的范围。实际迁移时必须同时更新 `aic/tools/exec/exec.go` 的工具描述、注入给 Agent 的指令说明和前端帮助，删除 bg run/output、等级与单指令调用示例；本轮不先修改运行中工具说明冒充功能已经上线。

不在本次改造中引入：单指令 argv 调用协议、第二个 Registry、强制 native 指令、通用权限框架、执行计划、审批凭据、运行时审批、暂停续跑、新任务服务，以及新的应用/站点权限模型。
