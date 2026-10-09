# Host 执行权限与原生沙箱

状态：2026-09-28 已实施（hosts-vsh-redesign 阶段 1-6）。本文替代旧等级检查流程；总契约见 [hosts-vsh-redesign.md](hosts-vsh-redesign.md)。

## 1. 边界

审批回答“用户是否允许本次脚本修改授权或免沙箱执行”，rules 回答“实际访问是否允许”，OS 沙箱约束原生子进程。三者不使用数字等级表达。

只有修改授权的 grant 和显式 nosandbox 触发审批。一个 Tool.Message 只做一次整单审批，通过即允许本次脚本中的 grant 和请求显式指定的 nosandbox，不分别审批。Browser/CUA 不审批；命令规则、资源规则和各服务的对象归属检查继续生效。允许 GUI 能力不等于文件沙箱能够约束另一个应用的全部行为。

## 2. 请求与授权修改

1. 验证 NATS 签名或 RTC 身份、目标、scope、会话绑定和有效期。
2. 构造不可由脚本修改的执行上下文。NATS 的 grant_approved 由服务端在用户明确批准该条 Tool.Message 后置为 true，无论由 grant 还是 nosandbox 触发审批；默认 false，受既有签名保护，不另建 grant 审批状态。
3. 执行脚本，按 exec 规则分发；虚拟指令优先，未命中才走受管的 PATH fallback。
4. FS/net/ssh 实际访问按对应 rules 检查；原生程序启动时生成 OS 沙箱。
5. 无论通过字面调用、变量、eval/source 还是嵌套脚本到达 grant，实际修改规则前都检查 grant_approved。没有批准就返回 permission_denied，不写规则、不暂停审批、不重放脚本。

grant status/help 不修改规则，不要求该标记。普通请求无需人工审批而获准执行时，grant_approved 仍为 false；由 nosandbox 触发的整单审批通过后同样置为 true。标记只在本次执行内继承，超时转后台不丢失，后续独立请求不继承；env、argv、stdin 都不能覆盖它。

这是使用现有可信请求传递一个整单审批事实，不是新的审批票据，也不是 pod 再做一次审批分类。RTC 仅信任已认证 owner 前端对本次请求的一次整体确认，不分项确认，不从连接身份推导所有请求均已批准；AI 走服务端工具入口。

授权单位是完整脚本及执行选项，审批文案须明确整单通过后允许本次脚本中的 grant 和请求显式指定的 nosandbox。改变目标、脚本、cwd 或选项需重新确认；不引入逐操作审批状态或票据。未指定 nosandbox 时，审批通过也仍使用沙箱。

核对旧实现时应注意：libs/host/execution.go 的 required 提升与 libs/host/engine_vsh.go 的预检依赖 Analyze，libs/vsh/cmds.go 的 grant 修改分支没有等价的上下文检查；不能把旧 granted_level 描述为已完整挡住动态 grant。

## 3. rules 与 grant

沿用现有 exec/fs/net/ssh 配置及匹配器，不为协议切换改名或重排规则。具体配置键以 cfg 的现有 Auth 定义为准；本设计不再复制过期的“所有 deny 永远优先”或“只允许已注册二进制”模型。

当前 FS 快照采用 first-match：会话 grant → 设备配置/永久规则 → 内建 deny → 便利根 → 默认值，设备配置组按其既有逻辑反转一次。显式 grant 因此可以改变此前 deny 的结果；普通请求的审批本身不改变任何规则。net/ssh 和 exec 继续按各自现有规则求值，不强行套用 FS 的行序。

fs 未命中规则时读默认开放，写按 fs_policy；deny/ro/rw、符号链接、rename/unlink 与条件写入由现有匹配器和文件服务处理。workdir 只决定路径解析，不授予访问权。云端用户根等领域硬边界不能通过 grant 绕过。

```sh
grant status
grant fs /work/extra
grant cmd git
grant net example.com:443 --permanent
grant ssh dev.example:22
```

grant 默认修改当前会话规则，--permanent 经校验、原子保存后才生效。grant cmd 只修改命令规则，不注册原生二进制。保留现有会话授权清理及永久配置生命周期，不在本轮另造存储层。

无效权限配置拒绝受影响的执行，不回退为 open，不靠 grant_approved 绕过配置错误。本机设置仍可修复配置。

## 4. 原生沙箱

原生程序默认按本次 rules 派生沙箱，不再用 granted_level 选择只读/可写 profile。进程内 FS/网络适配与原生沙箱应表达相同资源约束；平台无法落实必要隔离时必须拒绝，不能静默裸跑或忽略不支持的网络规则。

- macOS 继续使用 Seatbelt 后端，将有序资源约束转换成系统沙箱规则。
- Linux 继续使用 bubblewrap 后端，处理只读/可写挂载、拒绝范围及网络隔离。
- Windows 继续使用受限令牌和 ACL 后端；无法表达的文件或网络约束明确失败。

可写根的可落实性是唯一除外的方向：某个 `rw` 根授不上 ACL（典型：目录属主是
`Administrators`/`SYSTEM` 而当前用户只有 `Modify`——Modify 不含 WRITE_DAC）时，该根在沙箱内
降级为只读（更严方向，不会放权），日志按目录告警一次（vbox 的
`write-root unavailable in sandbox`），其余可写根与本次 exec 照常；每次 spawn 仍重试授权，
ACL 修好或目录重建后自动恢复。反方向的失败（deny 目标落不下、令牌/私有临时目录不可用）
仍然拒绝执行。授权侧可在 `grant fs` 那一刻用 `vbox.CheckFSGrantTarget`（原生态路径）预判：
回执带警告但仍然生效——进程内 fs 工具用宿主令牌写、不需要 WRITE_DAC，且目标还可能是
尚未创建的合法授权；`grant status` 对这类 `rw` 行标注 `[沙箱内不可写: …]`。

此处描述目标保证，不表示三个平台已经通过新规则的实测。Windows 文件服务自身的句柄检查、版本条件和原子提交继续由 hostfs 负责，不用原生进程沙箱替代。

nosandbox 是请求级显式选项，由发送端批准后进入可信执行上下文；只有物理 host 支持。它使本次原生进程脱离进程级隔离，不承诺该进程继续服从 fs/net 拒绝表；进程内服务和身份检查仍有效。Browser/CUA 的固定驱动调用不是脚本可伪造的免沙箱标记。

原生进程使用启动时快照，新授权供后续启动读取；不宣称规则变化能够实时修改已有进程的 OS 沙箱。配置、资源限制与进程组取消继续由既有实现承担，改变撤销语义需单独明示。

## 5. 生命周期与日志

普通 exec 不建 bg 任务，前台等待结束仍在运行时才登记原执行。bg 只负责 list/wait/kill；管理命令仍是可组合的普通脚本指令。任务和日志按 user_id + session_id 隔离，同会话 NATS/RTC 不另分权限域。

exec 外层将 stdout/stderr 分别写文件，始终返回已创建文件的路径，只截断响应预览。cancel 与 bg kill 取消实际脚本及受管进程；等待结束、网络断线不等于取消。具体预算、bg wait 和输出契约以主设计第 2 节为准。

## 6. 实施验收

- 无 grant_approved 的动态 grant 返回权限错误且不写规则（含 env 伪造与 ${g}nt 动态拼接用例，libs/vsh 测试覆盖）；grant/nosandbox 同时出现只审批一次，未指定 nosandbox 则仍使用沙箱。
- 模型参数和脚本环境不能伪造授权；修改签名信封的标记导致验签失败（protocol 包 hosts_nats 测试）；新 exec 不继承旧标记。
- 相同资源在 vsh、FS RPC 和原生进程中符合相同可实现规则，fallback 不绕过拒绝。
- stdout JSON 与 stderr 诊断分离，截断不丢日志；取消、超时转后台和文件关闭不存在重复执行或句柄丢失。
- macOS/Linux/Windows 分别在真实目标平台验证沙箱；仅编译通过不代表隔离成立（本项仍待真实平台实测）。
