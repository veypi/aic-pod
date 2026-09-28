# AIC Pod 设计文档

状态：2026-09-28 目标架构，已实施（阶段 1-6 完成）。详细契约以 [vsh、权限与宿主协议设计](hosts-vsh-redesign.md) 为准；[hosts-tools.md](hosts-tools.md) 描述当前实现。

## 概述

AIC Pod 是运行在用户设备上的能力代理。CLI 和 Desktop 共用 Go 后端，通过 NATS 接收服务端已获准发送的请求；已认证用户也可通过 RTC 直接操作。pod 验证身份、执行请求并检查本地资源规则，不运行人工审批流程。

命令执行只有完整脚本这一种输入。vsh 是面向 Agent 的 shell，bg、grant、browser、cua 和其他指令一样参与管道、重定向与控制流。FS 保持结构化数据接口，实时帧与输入留在 RTC 私有流端点。

## 模块职责

| 模块 | 职责 |
|---|---|
| cli / desktop | 客户端入口；Desktop 提供 Electron 窗口、本地设置和 Go 后端生命周期 |
| cfg / settings | 配置解析、校验、原子保存和设备绑定；不参与脚本审批判断 |
| libs/proto、libs/hostauth | 连接身份、签名、有效期、请求关联及可信执行上下文 |
| protocol/hosts_tools | 脚本、FS、取消请求及响应的数据结构，不注册业务指令 |
| protocol/hosts_nats、protocol/hosts_rtc | NATS 签名信封与 RTC 认证连接；共用脚本分发 |
| libs/host | 宿主装配、请求路由；exec 外层统一前台等待、后台登记和分流日志 |
| libs/vsh | vsh 集成，注册平台指令；保留虚拟指令优先与宿主 PATH fallback |
| libs/exec_procs | 原生子进程、OS 沙箱及进程组取消，不自建第二套输出契约 |
| libs/fsauth、libs/netauth 等 | 现有资源规则、会话 grant 和沙箱约束派生 |
| libs/hostfs | 文件、版本、字节源、上传与条件提交 |
| libs/browser、libs/cua | 页面与桌面领域状态；作为普通 vsh 指令调用这些服务 |
| libs/rtc、protocol/ui | RTC 连接与 UI 领域数据，不承担命令审批分级 |

继续使用现有包，不为这次改造增加权限服务、任务服务或第二个 Registry。旧 hosts_tool 声明体系在迁移时删除，不再维护 Method/Translate/AccessRules/RequiredLevel 或 DeviceCommand 投影。

## 客户端与配置

- CLI 面向 Windows、macOS、Linux，可用于个人设备、服务器和容器。
- Desktop 使用 Electron 远程页面与同一 Go 后端；本地设置通过受控 IPC 调用配置命令，不另建本地工具 HTTP 服务。Browser 由 Go 服务管理专用浏览器，CUA 由 Go 服务适配本机驱动。
- embedded/mobile 仍属未来适配，本提案不宣称已经实现。

CLI 与 Desktop 沿用当前配置文件及解析链：显式 flag → 环境变量 → 配置文件 → 默认值。保留 host/key/work_dir/exec_timeout/rtc 等现有配置，不为新协议重写配置系统。无效权限配置拒绝执行，不静默改成开放；持久修改经校验和原子写入。

执行身份和审批事实不能从脚本环境变量读取。配置层环境变量与 vsh 脚本 Env 是不同边界，不能混为一谈。

## 命令与发现

虚拟指令直接注册到 vsh；未命中时交宿主适配器按 PATH 查找原生程序，不逐个注册机器上的二进制。命令规则拒绝后不得继续 fallback。page 没有原生执行能力。

host/cloud 的 commands 只展示核心自定义能力；page 展示全部已注册指令。未出现在发现结果中不等于不可执行。用脚本执行 commands 和 `<command> --help` 获取帮助，不设独立 catalog 或单指令调用协议。

扩展业务只需实现普通 vsh 指令与服务，不再添加宿主方法表、等级表或 argv 翻译规则。viewer 所需的 --json 输出由命令自身维护稳定 schema 和契约测试，仍是 stdout 文本。

## 权限与审批

工具 enabled 和目标准入在 aic；exec/fs/net/ssh 的实际访问由执行端 rules 判断。普通请求获准发送不会改写 rules，也不会自动关闭沙箱。

只有 grant 授权修改和显式 nosandbox 触发审批，Browser/CUA 包括前台接管都不需要。一个 Tool.Message 沿用一个状态机，只做一次整单审批，通过即覆盖本次脚本的 grant 和请求显式指定的 nosandbox，不分项审批。发送端据整单批准结果在可信上下文置 grant_approved；现有签名覆盖该标记和完整请求。grant 的所有修改分支在实际写规则前检查它，缺失返回权限错误，不补审批。

grant_approved 不是等级、独立审批状态、审批票据或会话开关。无论审批由 grant 还是 nosandbox 触发，整单通过都设置它；它只属于被批准的完整脚本执行，随同一次运行的后台转换保留，不传给下一次 exec。批准不直接修改 rules，也不隐式打开未指定的 nosandbox。

资源判定和原生沙箱详见 [host_sandbox.md](host_sandbox.md)。删除旧的 granted_level 纵深比较并不意味着删除真实执行点的授权检查。

## 执行、后台与日志

普通 exec 直接运行，不先创建 bg 记录。前台等待到期仍未结束时，才将原执行登记到 bg 表、返回 ID；不重启脚本，不重置运行期限。bg 只有 list/wait/kill，不再有 run/output。

bg wait 共用当前 exec 剩余前台预算，提前返回目标状态，不独立创建等待任务。任务归属为 user_id + session_id；同一会话的 AI/NATS 与用户/RTC 互见互管，跨会话隔离。RTC 会话由认证票据绑定。

每次 exec 由外层分别保存完整 stdout、stderr；attrs.output 和 attrs.error_output 指向两个文件。content 和 attrs.stderr 分别返回有限预览；截断由 exec 外层处理，不改变日志或管道数据。转后台继续使用原文件，读历史输出使用普通 FS。

传输超时至少比前台等待多 5 秒；丢失回复不自动重跑。cancel 按 request_id、bg kill 按任务 ID 取消同一个实际执行及其受管子进程。断线或等待结束不等于取消，取消不回滚副作用。

## 协议与前端

保留 hosts_tools / hosts_nats / hosts_rtc 分工，升主版本整体切换，不设兼容层。NATS 信封继续校验身份、目标、scope、签名、nonce 和有效期；新增的 grant_approved 由既有签名保护，不增加第二次握手。

FS 仍使用领域 method/args；RTC 的 page.frames/page.input 是私有流，不进入 vsh 或 caps。移出目录不代表免除身份、页面归属和资源检查。

前端只提交普通脚本。动态值统一经共享 shellQuote 处理；viewer 使用稳定 --json 输出，等待整段脚本成功后仅解析 stdout，截断时读取 stdout 文件，不从混合文本中过滤诊断。

## 实施状态

阶段 1-6 已落地：可信上下文与 grant_approved、browser/cua 注册为 vsh 指令、exec 统一输出与超时转 bg、三协议切换与 RTC 私有流、aic 调用端与前端收口（shell_quote 单点转义、viewer 改 exec(script) 消费 --json）、旧体系删除。验证为 go build/vet/test 与 node 测试；真实平台的沙箱覆盖、取消和日志行为以实测为准。
