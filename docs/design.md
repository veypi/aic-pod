# AIC Pod 设计文档

状态：2026-09-28 目标架构，已实施（阶段 1-6 完成）。详细契约以 [vsh、权限与宿主协议设计](hosts-vsh-redesign.md) 为准；[hosts-tools.md](hosts-tools.md) 描述当前实现。

## 概述

AIC Pod 是运行在用户设备上的能力代理。CLI 和 Desktop 共用 Go 后端，通过 NATS 接收服务端已获准发送的请求；已认证用户也可通过 RTC 直接操作。pod 验证身份、执行请求并检查本地资源规则，不运行人工审批流程。

命令执行只有完整脚本这一种输入。vsh 是面向 Agent 的 shell，bg、grant、cua 与 skill 包命令（browser 等）和其他指令一样参与管道、重定向与控制流。FS 保持结构化数据接口，实时帧与输入留在 RTC 私有流端点。

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
| vbox（外部依赖 ivec/vbox） | 原生子进程托管、OS 沙箱及进程组取消，不自建第二套输出契约 |
| libs/fsauth、libs/netauth 等 | 现有资源规则、会话 grant 和沙箱约束派生 |
| libs/hostfs | 文件、版本、字节源、上传与条件提交 |
| libs/cua | 桌面领域状态；作为普通 vsh 指令调用 |
| libs/skillrun | skill 包在 pod 端的安装/注册/运行权威（v6）：manifest 校验、包名冲突、根命令注册、懒启动与 stream 路由 |
| skill-packages/ | skill 包源码（hello、browser）；仓库为源，构建产物落各包 cli/bin/（不入库） |
| libs/rtc、protocol/ui | RTC 连接与 UI 领域数据，不承担命令审批分级 |

继续使用现有包，不为这次改造增加权限服务、任务服务或第二个 Registry。旧 hosts_tool 声明体系在迁移时删除，不再维护 Method/Translate/AccessRules/RequiredLevel 或 DeviceCommand 投影。

## 客户端与配置

- CLI 面向 Windows、macOS、Linux，可用于个人设备、服务器和容器。
- Desktop 使用 Electron 远程页面与同一 Go 后端；本地设置通过受控 IPC 调用配置命令，不另建本地工具 HTTP 服务。Browser 由 Go 服务管理专用浏览器，CUA 由 Go 服务适配本机驱动。
- embedded/mobile 仍属未来适配，本提案不宣称已经实现。

CLI 与 Desktop 沿用当前配置文件及解析链：显式 flag → 环境变量 → 配置文件 → 默认值。保留 host/key/work_dir/exec_timeout/rtc 等现有配置，不为新协议重写配置系统。无效权限配置拒绝执行，不静默改成开放；持久修改经校验和原子写入。

执行身份和审批事实不能从脚本环境变量读取。配置层环境变量与 vsh 脚本 Env 是不同边界，不能混为一谈。

## 目录契约

pod 在设备上只有两根（2026-10-01 两根治理，三平台统一，无迁移无兼容——旧 UserConfigDir 配置丢弃，重配对即重建）：

| 根 | 路径 | 用途 |
|---|---|---|
| 设备状态根 | `$HOME/.aic` | pod 自持一切：config.yaml、aic.log、state.json、desktop.sock、browser/、sessions/{sid}、vsh/（引擎布局）、skills/（v6）、run/（v6 socket） |
| 用户工作区 | `$HOME/aic`（默认 work_dir，可改） | AI 的工作目录，沙箱可写 |

读写矩阵（默认 fs_policy 下工具/会话视角）：

- `$HOME/.aic` **可读不可写**——不进沙箱写白名单，也不是「公共可写区」；例外两条：`config.yaml` 在 deny 表（含凭证，不可读），`browser/` 在 deny 表（含全量 cookie，不可读写）。
- `sessions/{sid}` 由 fsauth 按会话显式授写（本会话便利根），是 .aic 下唯一可写子树。
- **权限门管 exec/工具，不管 pod 自身读写**：pod 的机械 IO（vsh 布局 stub、状态快照、日志）走未过门的 OS 通道（vshcore LayoutFS / 直接 os 调用），不依赖也不授予任何 .aic 白名单条目。`vsh/`（stub bin/home）对会话无写通道。
- 公共可写区 = 系统临时目录（os.TempDir + 平台 tempRoots）；跨会话保存请落 /tmp 或工作区。
- 该保护只覆盖默认拒写策略；fs_policy=open 或显式 grant 可覆盖（用户显式选择优先）。

单一事实源 = `cfg.StateDir()`；Electron 侧同路径推导（`home/.aic`）。

## 命令与发现

虚拟指令直接注册到 vsh；未命中时交宿主适配器按 PATH 查找原生程序，不逐个注册机器上的二进制。命令规则拒绝后不得继续 fallback。page 没有原生执行能力。

host/cloud 的 commands 只展示核心自定义能力；page 展示全部已注册指令。未出现在发现结果中不等于不可执行。用脚本执行 commands 和 `<command> --help` 获取帮助，不设独立 catalog 或单指令调用协议。

扩展业务只需实现普通 vsh 指令与服务，不再添加宿主方法表、等级表或 argv 翻译规则。viewer 所需的 --json 输出由命令自身维护稳定 schema 和契约测试，仍是 stdout 文本。

## skill 包（v6）与 browser 拆包（P5）

skill 包是 pod 能力的分发形态（契约：aic/docs/skill.md §9.2；权威实现 libs/skillrun）：

- 根命令 = 包名隐式（manifest 无 commands[]）；argv/stdin 全量透传 providers[0]；子命令与 --help 由包 CLI 自行实现。
- process 类 provider：每调用独立 vbox 沙箱进程；service 类：首调用懒启动 + bg 登记 + skillproc 拨号（SKILLPROC_SOCKET 经 env 注入 provider）；**service 一律 NoSandbox 原生权限边界**（2026-10-02 用户定：会话沙箱是给 AI 调用戴的，不套驻留服务；详见 aic/docs/skill.md §9.3 数据与文件访问）。
- **确保机制（P5 补）**：包命令被调用时若 manifest 含 service 类 provider，skillrun 先全部确保懒启动，并把 socket 路径注入 process provider 的 env——单 service 包同时给约定键 SKILLPROC_SOCKET，每个 service 恒给 SKILLPROC_SOCKET_\<ID 大写\>。process provider 经该 socket 拨号 svc。
- streams[]：二进制流端点由 service provider 经 stream.open 提供（帧负载 = 端点打开参数，原样透传由包自行解析）。端点解析是 OpenToolStream 唯一路径（skillrun.ResolveStreamEndpoint）：先 {包名}.{流名} 直查，再全端点名 = 流名全名；权限门 = 解析出的包名走 execAllowed（与 vsh 指令同一判定）。
- 安装来源：本地目录（skill install）或 NATS fetch zip；pod 启动扫描 ~/.aic/skills 自恢复（只认有效 .install.json）。

browser 自 P5 批①a 起从内建迁为 skill 包 skill-packages/browser/，行为不变是最高准则：

- **包布局**：cli/manifest.json（providers：main=process → cli/bin/browser，svc=service → cli/bin/browser-service；streams：page.frames/page.input → svc）；provider/browser = 能力内核（页面/动作/下载/上传/流，原内建 browser 包）；provider/chrome = CDP 传输与 Chrome 探测；provider/process = CLI（无状态 skillproc 转发器）；provider/service = svc（持有 Chrome 与全部状态的唯一进程）。构建 = 包内 build.sh → cli/bin/。
- **RPC 边界**：一切触碰 Chrome 的操作都在 svc；CLI 只把 argv/cwd 经 invoke 帧转发——子命令表、参数解析、JSON 输出契约都在 svc 的 Service.Run（单一事实源）。取消 = vbox 杀 CLI 进程组 → 连接断开 → svc 取消该连接的 invoke。stream 的 stream id = 连接身份（输入租约持有者判定）。
- **端点名映射**：manifest StreamDecl.Name 直接声明全名 page.frames/page.input（端点名 = 流名全名，v5 前端契约不变；不引入包名别名机制）。
- **配置下发**：pod 不再持有 browser 配置项（cfg browser_* 与 host Options 字段删除）；svc 读自身环境（vbox 子进程继承 pod env 清洗后 + 显式注入）：AIC_BROWSER_PATH（显式覆盖，最高优先）/ AIC_BROWSER_BUNDLE_DIR（打包器提示：目录内含 Chrome for Testing，{platform}-{arch}/ 布局，desktop 注入 resources/browser 或 vendor/browser）、AIC_BROWSER_STATE_DIR（默认 $HOME/.aic/browser）、AIC_BROWSER_WIDTH / AIC_BROWSER_HEIGHT（默认 1280/720）。Chrome 可执行文件探测全在 Go provider（chrome.Resolve）：AIC_BROWSER_PATH > bundle dir > 系统候选（macOS .app / Windows PROGRAMFILES 系 / Linux PATH 名）。
- **单用户语义**（P3/todo 决策随本批落地）：v5 的文件门回调删除（文件权限 = 进程沙箱）；caller/subject 身份模型删除（Service.owner、页面/下载/元素引用的 subject 过滤、运行中 auth Validate）。租约语义收紧：自动化动作遇输入租约一律 control_busy——v5 的「同连接 viewer 就地终租」路径在生产不可达（vsh 调用身份是 vsh:owner:session，永不等于 RTC stream 连接 id），删除不改变可观察行为。
- **路径解析**：page.upload / download.export 的相对路径按 invoke cwd（CLI 进程 cwd = vsh 会话 workdir）绝对化；v5 相对 pod 进程 cwd 解析是隐性缺陷，随拆包修正。
- **首装形态**：仓库 skill-packages/browser/ 为源，build.sh 构建后经 skill install 本地目录安装；desktop 形态由 build.sh 产出 browser.zip 随包进 resources，pod 启动经 AIC_BUILTIN_SKILLS 首跑预装（builtin 来源，kind=builtin id=包名，Name/Version 取自 zip 内 SKILL.md frontmatter；同源同版本幂等跳过，异源同名显式报错与 Download 同语义，单项失败只记日志不阻断启动）。
- **包 UI（P5 批①b1）**：`ui/` = 设备浏览器查看器（原 aic `ui/page/local/browser.html` + `ui/hosts/browser*.js` + `ui/os/browser-*.js` 迁入，入口 index.html；shell_quote.js 复制入包保持自包含）。SKILL.md frontmatter 声明 `ui: [{path: index.html, handles: [http, https]}]`——平台 OS `open` 遇 http(s) 外链经通用 handles 机制打开本包页面（窗口身份 = 包页面裸 URL，单实例；`?url=` 深链与 pageDesc `open` 指令 = 多标签入口，页面侧 `openLink`：全设备同 URL 窗口聚焦复用，否则选中/首台就绪设备新建标签）。包页面经平台固定 env.js root 链用 `$hosts`/`$message` 等宿主服务；`ui/langs.json` 自带 browser.* 双语键（vhtml i18n 管道维护）。测试：`cd ui && node --test`（32 例，无 aic 基建依赖）。
- **资源回收语义（P5 批②）**：元素引用从「2 分钟墙钟 + 满 4096 报 overloaded」改纯序 LRU（无墙钟）——单页容量 4096，observe 插入队首、resolve 命中提队首、满容从队尾淘汰最久未用（淘汰替代等待，不再报错）；失效只靠导航 document 检查（stale_ref / observation_changed 语义不变）。下载删除 DownloadTTL——回收 = 页面关闭（进行中走 CDP 取消，删文件+删记录）+ 容量淘汰（触发点 = 新下载登记：记录数达 128 或总字节预留不出一份单文件上限时，按创建先后淘汰最旧的非进行中记录；inProgress 永不被动淘汰）。进行中超 MaxDownloadBytes/MaxTotalDownloadBytes 仍即取消；上限只防卡死与无限堆积。

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

FS 仍使用领域 method/args；RTC 的 page.frames/page.input 是私有流，不进入 vsh 或 caps（v6 P5 起由 browser 包 manifest streams[] 声明，经 skillproc 桥接——端点名不变）。移出目录不代表免除身份、页面归属和资源检查。

前端只提交普通脚本。动态值统一经共享 shellQuote 处理；viewer 使用稳定 --json 输出，等待整段脚本成功后仅解析 stdout，截断时读取 stdout 文件，不从混合文本中过滤诊断。

## 实施状态

阶段 1-6 已落地：可信上下文与 grant_approved、browser/cua 注册为 vsh 指令、exec 统一输出与超时转 bg、三协议切换与 RTC 私有流、aic 调用端与前端收口（shell_quote 单点转义、viewer 改 exec(script) 消费 --json）、旧体系删除。v6 已落地：skill 包机制（P0-P3：skillrun 安装/注册/懒启动/stream 泛化）与 browser 拆包（P5 批①a：browser 迁为 skill-packages/browser，pod 侧硬编码全部移除）。验证为 go build/vet/test 与 node 测试；真实平台的沙箱覆盖、取消和日志行为以实测为准。
