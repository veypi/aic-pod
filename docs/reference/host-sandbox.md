# Host 执行权限与原生沙箱

状态：2026-10-26 现行权限契约；统一命令、策略核心、Manager 缓存与 darwin 代理已实现，实机与发布收尾见 [../development/todo.md](../development/todo.md)。总架构见 [../architecture/overview.md](../architecture/overview.md)，调用契约见 [hosts-tools.md](hosts-tools.md)。

## 1. 边界

审批回答“用户是否允许本次脚本修改授权或免沙箱执行”，`exec_policy` 回答“是否允许调用这个命令实例”，资源 rules 回答“命令实现获得调用权后，实际访问是否允许”，vbox 只约束 vbox 支撑命令启动的原生子进程。它们不使用数字等级表达，也不互相替代。

所有注册命令都先经过 vsh 的同一个 `exec_policy` 准入点。Go 内建、Pod 虚拟命令、MCP 服务命令和 vbox 支撑命令没有不同的 exec 准入通道。

只有修改授权的 grant 和显式 nosandbox 触发审批。一个 Tool.Message 只做一次整单审批，通过即允许本次脚本中的 grant 和请求显式指定的 nosandbox，不分别审批。Browser/CUA 不审批；命令规则、资源规则和各服务的对象归属检查继续生效。允许 GUI 能力不等于文件沙箱能够约束另一个应用的全部行为。

## 2. 请求与授权修改

1. 验证 NATS 签名或 RTC 身份、目标、scope、会话绑定和有效期。
2. 构造不可由脚本修改的执行上下文。NATS 的 grant_approved 由服务端在用户明确批准该条 Tool.Message 后置为 true，无论由 grant 还是 nosandbox 触发审批；默认 false，受既有签名保护，不另建 grant 审批状态。
3. vsh 解析脚本并得到命令实例，对所有命令统一执行 `exec_policy`；拒绝时不调用命令实现。
4. 准入通过后，命令实现落实自身策略：`ssh` 使用 `ssh_policy`，`curl` 使用 `net_policy`，vbox 支撑命令把 FS/NET 快照交给 vbox 后启动原生进程。
5. 无论通过字面调用、变量、eval/source 还是嵌套脚本到达 grant，实际修改规则前都检查 grant_approved。没有批准就返回 permission_denied，不写规则、不暂停审批、不重放脚本。

grant status/help 不修改规则，不要求该标记。普通请求无需人工审批而获准执行时，grant_approved 仍为 false；由 nosandbox 触发的整单审批通过后同样置为 true。标记只在本次执行内继承，超时转后台不丢失，后续独立请求不继承；env、argv、stdin 都不能覆盖它。

这是使用现有可信请求传递一个整单审批事实，不是新的审批票据，也不是 pod 再做一次审批分类。RTC 仅信任已认证 owner 前端对本次请求的一次整体确认，不分项确认，不从连接身份推导所有请求均已批准；AI 走服务端工具入口。

授权单位是完整脚本及执行选项，审批文案须明确整单通过后允许本次脚本中的 grant 和请求显式指定的 nosandbox。改变目标、脚本、cwd 或选项需重新确认；不引入逐操作审批状态或票据。未指定 nosandbox 时，审批通过也仍使用沙箱。

核对旧实现时应注意：libs/host/execution.go 的 required 提升与 libs/host/engine_vsh.go 的预检依赖 Analyze，libs/vsh/cmds.go 的 grant 修改分支没有等价的上下文检查；不能把旧 granted_level 描述为已完整挡住动态 grant。

## 3. rules 与 grant

Pod 统一管理 exec/fs/net/ssh 配置及会话快照，但各配置只发给声明它的消费者：vsh 接收 exec，vbox 支撑命令接收 FS/NET，`curl` 接收 net，`ssh` 接收 ssh。具体配置键以 cfg 的 Auth 定义为准。

所有注册命令都由 exec 规则统一准入。`grant cmd` 修改 exec 规则，不注册命令，也不改变命令内部资源策略。`ssh_policy` 只约束 Pod SSH 族命令，不约束 `/bin/ssh`；`net_policy` 分别约束 `curl` 和 vbox 支撑命令，但不通过 `tool:` 或其他前缀分流。

当前 FS/NET 规则采用 `level + 原表序`：高 level 优先，同 level 保持原表首命中。Pod 的 level 赋值契约如下：

- AI 申请、人工 session、`grant --permanent`、便利根和 `.git/.aws` protected 行都是 level 0；永久化不是提权。
- Pod 内建 hard protection 编译为 level 1；普通凭证 deny 与 `.git/.aws` protected 行仍保持 level 0。
- 所有者直接编辑配置可以写 `!` / `!!`；AI 来源的规则写入强制 clamp 到 level 0，配置写保护必须堵住 AI 自行提权的路径。
- `Class` 只组织规则，不推导 level。快照仍按 temp → base → protected → convenience 拼接，level 负责跨层优先级。
- hard list 的首批具体范围仍需所有者确认；候选是 Pod 配置、设备密钥、`ssh_config` 和 browser 状态。

显式 grant 因此可以在其 level 和表序允许的范围内改变此前 deny 的结果；普通请求的审批本身不改变任何规则。fs 未命中规则时读默认开放，写按 fs_policy；deny/ro/rw、符号链接、rename/unlink 与条件写入由匹配器和文件服务处理。workdir 只决定路径解析，不授予访问权。云端用户根等领域硬边界不能通过 grant 绕过。

```sh
grant status
grant fs /work/extra
grant cmd git
grant net example.com:443 --permanent
grant ssh dev.example:22
```

grant 默认修改当前会话规则，--permanent 经校验、原子保存后才生效。grant cmd 只修改统一的 exec 准入规则，不注册原生二进制。会话授权驻留内存、随进程重启失效——当前没有运行期清理点（会话结束后授权项仍留在表内，长运行进程随会话数缓慢累积，sid 随机不复用、无越权面）；永久配置生命周期维持现状，不在本轮另造存储层。

无效权限配置拒绝依赖该配置域的执行，不回退为 open，不靠 grant_approved 绕过配置错误，也不把无关命令全部禁用。本机设置仍可修复配置。语法错误与平台能力不支持是不同错误。

## 4. vbox 支撑命令与原生沙箱

vbox 支撑命令先与其他命令一样通过 `exec_policy`，再由其命令实现把完整 FS/NET 快照交给 vbox。vsh 不感知这一过程，原生程序也没有独立的 exec 准入通道或 basename 特殊规则。

- macOS 使用 Seatbelt 后端，将有序资源约束转换成系统沙箱规则。
- Linux 使用 bubblewrap 后端，处理只读/可写挂载、拒绝范围及网络隔离。
- Windows 使用受限令牌和 ACL 后端；无法表达的文件或网络约束明确失败。

可写根的可落实性是唯一除外的方向：某个 `rw` 根授不上 ACL（典型：目录属主是
`Administrators`/`SYSTEM` 而当前用户只有 `Modify`——Modify 不含 WRITE_DAC）时，该根在沙箱内
降级为只读（更严方向，不会放权），日志按目录告警一次（vbox 的
`write-root unavailable in sandbox`），其余可写根与本次 exec 照常；每次 spawn 仍重试授权，
ACL 修好或目录重建后自动恢复。反方向的失败（deny 目标落不下、令牌/私有临时目录不可用）
仍然拒绝执行。授权侧可在 `grant fs` 那一刻用 `vbox.CheckFSGrantTarget`（原生态路径）预判：
回执带警告但仍然生效——进程内 fs 工具用宿主令牌写、不需要 WRITE_DAC，且目标还可能是
尚未创建的合法授权；`grant status` 对这类 `rw` 行标注 `[沙箱内不可写: …]`。

net 快照不再包含 `tool:` / `ToolOnly`，也不存在 `netSnapshotForNative` 的执行层过滤。
配置语法校验与平台能力校验分开：配置可以合法表达目标语义，但当前 vbox 后端无法完整落实时，
对应 vbox 支撑命令在启动前返回“不支持”，进程不执行。不过滤规则、不改走 `curl` 或其他
命令实现，也不把该能力错误扩大成全部设备工具不可用。

`grant status` / `grant net` 可以展示 kernel/proxy landings 和“不支持”诊断，但诊断不改变
规则含义。deny 行落不下仍拒绝；沙箱绝不比策略更宽。此处描述现行保证；Linux/Windows
新规则的实机验证仍待完成。Windows 文件服务自身的句柄检查、版本条件和原子提交继续由 hostfs
负责，不用原生进程沙箱替代。

darwin 在 confined 且 `net_policy=deny` 时由 vbox 启动按完整 netKey 复用的 HTTP/SOCKS5
出站代理：loopback 直连仍由 Seatbelt 逐条控制，非 loopback 规则由代理按当次完整快照
执行。open 与 nosandbox 不创建代理。`net_upstream_proxy` 是所有者显式配置的可选
`http://` / `socks5://` / `socks5h://` 上游，经 `AuthCfg` 和 `compiledAuth` 进入 net
快照及 netKey；SSH 快照不携带它，也不从设备环境继承代理。子进程的代理变量由 vbox
最后强制设置，调用方 env 不能替换内部 endpoint。

nosandbox 是请求级显式选项，由发送端批准后进入可信执行上下文；只有物理 host 支持。它使本次原生进程脱离进程级隔离，不承诺该进程继续服从 fs/net 拒绝表；命令的 exec 准入、进程内服务和身份检查仍有效，不为 SSH/Git 等程序设置例外。Browser/CUA 的固定驱动调用不是脚本可伪造的免沙箱标记。

原生进程使用启动时快照，新授权供后续启动读取；不宣称规则变化能够实时修改已有进程的 OS 沙箱。配置、资源限制与进程组取消继续由既有实现承担，改变撤销语义需单独明示。

## 5. 生命周期与日志

普通 exec 不建 bg 任务，前台等待结束仍在运行时才登记原执行。bg 只负责 list/wait/kill；管理命令仍是可组合的普通脚本指令。任务和日志按 user_id + session_id 隔离，同会话 NATS/RTC 不另分权限域。

exec 外层将 stdout/stderr 分别写文件，始终返回已创建文件的路径，只截断响应预览。cancel 与 bg kill 取消实际脚本及受管进程；等待结束、网络断线不等于取消。具体预算、bg wait 和输出契约以主设计第 2 节为准。

## 6. 实施验收

- 无 grant_approved 的动态 grant 返回权限错误且不写规则（含 env 伪造与 ${g}nt 动态拼接用例，libs/vsh 测试覆盖）；grant/nosandbox 同时出现只审批一次，未指定 nosandbox 则仍使用沙箱。
- 模型参数和脚本环境不能伪造授权；修改签名信封的标记导致验签失败（protocol 包 hosts_nats 测试）；新 exec 不继承旧标记。
- 所有注册命令（含 SSH 族、curl、MCP、grant、bg/commands 和 vbox 支撑命令）经过同一个 exec 准入点；拒绝时实现未调用。
- `ssh` 与 `/bin/ssh` 可以使用不同实现和内部策略，但 exec 决策结构相同；绝对路径不触发 SSH 名称门或 gitx 拦截。
- `net_policy` 分别由 curl 和 vbox 支撑命令完整消费，不存在 `tool:` 过滤；vbox 不支持只影响对应命令，fallback 不绕过拒绝。
- stdout JSON 与 stderr 诊断分离，截断不丢日志；取消、超时转后台和文件关闭不存在重复执行或句柄丢失。
- macOS/Linux/Windows 分别在真实目标平台验证沙箱；仅编译通过不代表隔离成立（本项仍待真实平台实测）。
