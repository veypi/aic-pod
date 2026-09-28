# TODO：vsh、权限与宿主协议迁移

更新：2026-09-28。依据 [hosts-vsh-redesign.md](docs/hosts-vsh-redesign.md) 制定。阶段 1-6 已实施并通过 go build/vet/test 与 node 测试；阶段 7（联调与真实平台验收）待服务端/pod 重启后进行。设计有冲突时以该文档为准；实现和对应验收都通过后才勾选。

旧 Browser/三协议计划及当时进度保存在 [历史 TODO](docs/todo-browser-2026-09-21.md)，不再与本轮要求混用。

## 范围与顺序

- 只改现有执行、授权和接入链路，不新增执行框架、通用工具 Registry、权限 DSL 或任务服务。
- 外部执行入口只有完整脚本；Browser/CUA/bg 是普通 vsh 指令。保留 PATH fallback，不注册每个原生程序，不增加 argv/单指令 RPC。
- 审批与权限分开：grant 授权修改或显式 nosandbox 触发同一个 Tool.Message 的整单审批，只审批一次，通过即覆盖本次请求的两者；实际资源访问仍按 rules，Browser/CUA 不增加审批。
- FS 保持数据面；stream 只走 RTC，不进 vsh、commands、caps 或 exec 输出日志。
- 按 1 → 2 → 3 → 4 → 5 → 6 → 7 推进。阶段是开发顺序，不是独立发布版本；aic-pod、aic 服务端与前端联合切换新主版本，不做兼容分支。
- 主要修改 aic-pod 及相邻 aic 仓库；vsh 核心优先复用现有能力，仅在确有接口缺口时调整，不改变其默认不执行宿主程序的边界。

## 1. 可信上下文与 grant 安全边界

涉及：`libs/vsh`、`libs/host/grant.go`、`libs/host/engine_vsh.go`、`libs/proto`；aic 的 `tools/exec`、工具配置/审批调用链、cloud/page 执行上下文。

- [x] 将可信 user/session、grant_approved 和 nosandbox 执行选项从接入端传入本次执行上下文；不从脚本可修改的 `AIC_VSH_LEVEL / SESSION / NOSANDBOX` 读取授权信息。grant_approved 默认 false，不暴露为模型参数。
- [x] 所有 grant 修改 rules 的入口统一检查 grant_approved，包括临时和永久授权；status/help 不要求批准。未获准直接 permission_denied，不暂停、不发起审批、不修改规则。
- [x] aic 在发送前检查工具启用/目标；grant 修改或显式 nosandbox 任一触发审批，都使用 Tool.Message 已有状态机做一次整单审批，不分别维护审批状态。通过后设置 grant_approved=true，并允许请求显式指定的 nosandbox；静态分析只用于提前提示，复用 CLI 参数解析，不作为安全证明。审批绑定目标、完整脚本、cwd 和执行选项；修改后重新确认。
- [x] 本次脚本的嵌套执行、管道和转后台继承同一个批准事实；后续独立 exec 不继承。grant_approved 由整单审批结果导出，不是第二次审批；批准不直接修改 rules，也不隐式打开未指定的 nosandbox。
- [x] 删除本轮工具配置和执行链路的数字等级判定，配置只表达启用与目标；保持 exec/fs/net/ssh rules 的匹配顺序、默认值和原有授权边界。权限错误明确提示如何用 grant 申请，不自动重跑原操作。
- [x] 原生执行按 rules 派生沙箱，无法隔离时明确失败；免沙箱不能冒充仍有原生 fs/net 隔离，进程内资源服务仍检查 rules。

验收：无批准的字面 grant、变量拼接、命令替换、eval/source 和嵌套脚本均不能改规则；伪造 env 无效；同一消息同时涉及 grant/nosandbox 只审批一次，由任一触发的整单批准都设置 grant_approved；未指定 nosandbox 仍使用沙箱，单次批准不串到下一次执行。可信信封与 RTC 入口在阶段 4 联调。

## 2. Browser/CUA 直接成为 vsh 指令

涉及：`libs/vsh/engine.go`、`libs/vsh/cmds.go`、`libs/vsh/native.go`、`libs/host`、`libs/browser`、`libs/cua`；aic 的 cloud/page 指令装配。

- [x] 沿用 vsh 自身注册机制，直接注册 browser/cua，与 commands/grant/bg 一样参与完整脚本执行；各自提供 CLI 参数解析、--help、标准流与退出码，不通过旧 hosts_tool call/Translate 中转。
- [x] 同一个 Browser 服务实例供 vsh 指令和 RTC stream 使用，共享页面及业务状态；在宿主装配点集中连接两类入口，可用普通注册函数收拢代码，不增加工具描述层或第二套业务实例。服务生命周期不归单次 exec/bg 管理。
- [x] 保留“已注册指令 → PATH/显式程序路径 → command not found”的查找顺序；已命中但被 rules 拒绝时禁止 fallback。移除原生二进制逐个注册，grant cmd 只改规则。
- [x] commands 直接读取 vsh 注册信息：host/cloud 只展示实际提供的核心自定义指令，page 展示全部；不枚举 PATH，不把展示过滤变成执行白名单，也不为此删除常见虚拟指令实现。
- [x] viewer 使用的 Browser/CUA 指令提供稳定 --json：stdout 只放结果，诊断写 stderr；沿用 PageInfo[] / UI Result，并明确 viewer 所用字段的 schema、空值形状及黄金样例。schema 留在业务包和测试，不回到方法目录。

验收：`browser page.list --json | jq '.'`、`cua --help | grep usage`、原生程序与虚拟指令混合脚本正常；变量、条件、循环、管道、重定向遵循同一套 shell 语义；未列入 commands 的允许指令仍可执行。

## 3. exec 统一输出，等待超时才转 bg

涉及：`libs/host/engine_vsh.go`、`libs/host/execution.go`、`libs/exec_procs`、`libs/vsh`；aic 的 `tools/exec/cloud.go`、执行日志和 page 执行实现。

- [x] 每次 exec 直接启动脚本，只保存运行上下文、完成/取消句柄和日志，不创建 bg 记录或 bg ID；request_id 只用于关联与日志命名。
- [x] 前台等待到期仍未完成时，原执行只登记一次 bg 并返回 background/id；不重启、不重放、不重置整次运行期限，处理完成与超时竞争。
- [x] 由 exec 接入层为所有执行持续保存 stdout/stderr 两个文件（引擎 stdio 接线，引擎不感知日志文件）；截断只针对 NATS（AI 消费）：content 和 attrs.stderr 只放有界预览，任一预览超限设置 truncated；RTC 直连全量返回（无 truncated 标记），更多数据经 fs 读日志。attrs.output/error_output 返回可读路径。沿用 attrs 字符串值约定，不在内存保留全量输出。
- [x] 只截断预览，不截断日志或管道数据；尊重用户显式重定向。执行失败保留已创建的日志地址，创建/写入失败明确报错；转后台沿用原文件，实际结束才关闭，不由内部指令自行记日志或截断。
- [x] bg 仅保留 list/wait/kill；表内只存状态、归属、退出码、日志路径和管理句柄，不留第二份输出。移除 run/output；沿用有界完成记录清理，不做持久恢复。转后台容量不足则取消原执行并返回错误及日志路径。
- [x] bg list/wait/kill 本身不预占 bg 名额；满额或其他脚本运行时，查询和取消仍可执行，包括带管道的组合脚本。脚本内部 `&` 不逐条登记为 bg。（容量检查前先惰性结算已完成任务，修复“满额无法 bg list 解锁”死锁。）
- [x] bg wait 共用本次 exec 剩余前台预算，最多等待“指定秒数”与“剩余预算减 1 秒”的较小值；预算不足或在后台时只查询，禁止等自己。不独立登记等待任务；显式循环/其他耗时仍按整段脚本超时处理。
- [x] bg list/wait 提供设计规定的 --json 状态、日志路径和终态 exit_code，不回放目标日志；查询 exec 的 attrs 仍指向查询自身日志。
- [x] cancel(request_id) 和 bg kill 指向同一实际执行的取消句柄，终止脚本及受管子进程，保留转后台期间的关联；实际结束后才报终态。断线/停止等待不是取消，取消不回滚副作用。
- [x] 任务与 cancel 统一按 `(user_id, session_id)` 检查归属；同会话 NATS/RTC 互见互管，跨会话隔离，无会话调用进入独立 manual 范围。日志是普通文件：读写由 FS rules 决定，按会话分目录只是整理，不构成权限边界，不加日志 ACL 或隐藏权限层。

验收：短 exec 零 bg 记录，长 exec 只转后台一次；等待链不膨胀；满额能查询/取消；大输出完整落盘且预览有界；stderr 不污染 JSON；host/cloud 使用同一执行语义（page 无长时间任务，不接 bg——2026-09-28 用户裁定）。

## 4. 三协议切换与 RTC 私有流

涉及：`protocol/hosts_tools`、`protocol/hosts_nats`、`protocol/hosts_rtc`、`libs/proto`、`libs/host`、`libs/rtc`、FS 接入；aic 的 `libs/host`、`libs/natsauth`、`api/hosts`。

- [x] 升三协议主版本。hosts_tools 只保留 exec(script)、fs(method,args)、cancel 请求及 result/error；保留 cwd、stdin、等待时限、nosandbox，校验 action 与载荷互斥；删除命令 call、argv、catalog 和 exec.run 包装。
- [x] NATS 验签后与 RTC 使用同一个普通请求分发入口；保留目标、身份、会话、scope、nonce 和有效期校验。信封移除 granted_level，加入默认 false 的 grant_approved，既有 HMAC 覆盖完整请求和该标记；更新签名测试向量。
- [x] pod 不再重新分类审批或返回 waiting/approval_required；只在 grant 修改入口检查整单已批准事实。RTC owner 也对本次请求整体确认一次，确认元信息只作用于本次请求，不能因连接认证通过就全量批准；agent 不走 owner 确认入口。
- [x] RTC session_id 由认证票据绑定，不能信任未经核验的请求值；保留认证、续期、撤销和连接清理机制。
- [x] page.frames/page.input 改为 RTC 私有 stream 端点，直接绑定阶段 2 的同一 Browser 服务；明确 stream.open 的端点、打开参数和 DataChannel 绑定，移除对命令 Invocation/方法目录的依赖。沿用 Send/Recv/Close 字节流，不进 vsh、commands 或 caps。
- [x] 流继续检查调用者、页面归属、能力 rules、租期和人工控制；失效关闭流并清理输入，保留背压与取消。帧/输入直接经 DataChannel 传输，不产生 exec 日志或 bg 任务。
- [x] FS 直达现有文件服务，保留路径、版本、上传、字节源、条件提交及 fs-only 代理范围；与 vsh 文件访问共用资源规则，不借迁移改变文件行为。
- [x] 前台有效等待 W 与各层传输超时 T 协调为 `T >= W + 5 秒`，考虑排队开销、授权期限和运行期限；丢失回复报告结果未知，利用已有请求关联/去重核实，不自动换 ID 重放脚本，不新增执行 ID/epoch 机制。

验收：同一脚本经 NATS/RTC 结果一致；篡改 grant_approved 验签失败；伪造会话/跨会话取消被拒；fs-only 不能执行；stream 不出现在指令目录，且与 CLI 操作同一页面。

## 5. aic 调用端和前端收口

涉及：aic 的 `tools/exec`、`libs/host`、`ui/hosts/tools.js`、`ui/hosts/browser.js`、CUA viewer、`ui/assets/libs/page.js`、`ui/ai/msg/tool_utils.js`。

- [x] 服务端统一提交完整脚本与执行选项，移除 DeviceCommand/DeviceMethod 驱动的单指令翻译及审批分级；cloud/page 与 host 使用同一授权、输出、bg 和取消契约，不保留旧执行分支。
- [x] 新增共享 `ui/assets/libs/shell_quote.js` 的 shellQuote(value)，由共享基础库单点维护/审计；所有 viewer 动态参数使用它，固定命令/flag/运算符不接受外部替换。处理空串、引号、换行和 Unicode，拒绝 NUL，另用带值 flag/`--` 处理选项注入。
- [x] Browser/CUA 普通操作从 method+args 改为 exec(script)，按 --json 契约消费结果；移除对 catalog 的依赖。viewer 不实现另一套命令调用协议。
- [x] 在现有共享客户端调用层集中处理完成、退出码和 stdout 取得：成功响应直接解析 --json（RTC 不截断、不转后台，无兜底分支）；错误、非零退出和格式异常显式报出，不解析半截 JSON；传输异常不自动重跑操作。
- [x] viewer 校验成功 JSON 的必需字段和类型，未知字段可忽略；错误、非零退出和格式异常显式显示，不当作成功数据。
- [x] openStream 接入阶段 4 的 RTC 私有端点；帧解码和实时输入保持二进制通道路径，不套 exec，不经过 stdout/output 文件。FS 客户端沿用数据面调用。
- [x] 删除 tool_utils 的旧方法翻译及兼容分支，保留其他仍使用的展示工具；同步普通完成、后台、取消、错误和分流输出展示。

验收：转义测试覆盖反斜杠、美元符、反引号、换行、通配符、分号、命令替换，并在 vsh 中验证参数原样还原且无额外执行；viewer 覆盖直接返回、截断、后台、stderr、失败和断线；帧/输入操作不触发日志读取。

## 6. 删除旧体系，同步说明

涉及：aic-pod 与 aic 的旧声明、发现、配置、工具说明及测试。

- [x] 删除 hosts_tool 的 Command/Method/Bind/Translate/AccessRules/RequiredLevel 声明与分发体系；先迁出仍需的最小调用上下文、流接口和 FS 接入，不误删业务服务、认证、流清理或文件能力，也不原样改名重建一套。
- [x] 清除旧 caps 方法投影、catalog、DeviceCommand、原生逐个注册、Background 方法标记、bg run/output、重复任务表/输出缓冲及不可达兼容入口。
- [x] 清除本轮协议和执行链路的 granted_level、Level=9 重发和环境变量授权依赖；更新相关配置/UI、签名和测试，不让数字等级以旁路形式继续影响执行。
- [x] 实际切换时同步 `tools/exec/exec.go` 工具描述、Agent 指令注入、前端帮助和 aic 的 `docs/instruction_sets_v2.md`；示例只展示脚本、commands/--help、bg list/wait/kill 和新输出约定，不提前宣称功能上线。
- [x] 校准 `docs/design.md`、`docs/host_sandbox.md`、`docs/hosts-tools.md`、`docs/hosts-protocols-proposal.md` 与前端 `ui/hosts/README.md`；旧规范明确归档或替换，不能留下两份同时生效的协议。保留 Browser 自身业务语义，移除与新执行模型冲突的描述。

验收：活动代码、工具说明和测试不再引用旧调用/等级/后台模型；历史文档允许保留旧词，但必须明确失效。各端没有兼容分支，没有第二个命令目录或工具注册框架。

## 7. 联调与交付验收

- [ ] 完成阶段 1–6 的单元、契约与集成测试；分别运行 aic-pod 和 aic 的 Go 测试、前端相关测试，对后台登记、取消、流关闭等并发路径执行 race 检查。
- [ ] 端到端覆盖 host/cloud/page、NATS/RTC、前台/后台，以及“权限拒绝 → 明确提交 grant → 用户批准 → 重新执行”；确认正常 Browser/CUA 操作不出现审批。
- [ ] 实测完整 Browser 链路：脚本创建/查询/操作页面，RTC 观看/输入操作同一页面，人工控制冲突、断线和页面关闭正确清理；同时回归 CUA 和 FS/proxy，不因协议迁移破坏既有业务。
- [ ] 验证取消终止受管子进程、沙箱不可用时拒绝执行、同会话互通与跨会话隔离；实测结果分别记录，交叉编译不能替代三平台原生运行验收。
- [ ] 记录本次已测平台、依赖和未覆盖项；服务端、pod、前端共同升级，旧主版本明确不支持。更新设计状态和本 TODO，只勾选有测试依据的完成项。

交付边界：本轮不新增 browser 脚本 SDK/Goja 编排、多 profile、远程 CDP、动态插件或新的 GUI 权限模型。历史计划中的独立 Browser 跨平台/打包待办继续保留，不以本轮协议迁移完成代替它们的验收。
