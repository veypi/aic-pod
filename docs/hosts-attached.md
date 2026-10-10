# 附属设备（hosts_relay/1）

草案：2026-10-08。关联：[设备 MCP](hosts-tools.md)、[managed-ssh](managed-ssh.md)、[host 沙箱](host_sandbox.md)、[架构](design.md)；平台侧落地时同步 aic/docs/nats.md。

附属设备（attached device）是不持有平台凭证、不连接 NATS 的执行端，经所属主设备（master）的 SSH 中继接入平台。附属侧跑同一个 aic-pod 二进制的 `aic slave` 模式，dispatch/hostfs/vsh/vbox/权限规则全部零改动复用——**本设计不是在 SSH 上翻译协议，而是把 pod 的 dispatch 入口直接接到一条 SSH stdio 多路复用通道上**。

## 1. 动机与定位

为什么不让附属设备直接跑完整 pod（零代码方案）？成立的前提是以下约束至少其一：

- **网络不可达**：附属设备在私网/内网，只能被主设备 SSH 到，无法直连平台 NATS；
- **凭证不落地**：设备是共享/ ephemeral 机器，所有者不愿在其上发放平台 host secret（secret 一旦泄露吊销成本高于一把可随时删除的 SSH 授权钥）；
- **接入轻量化**：附属设备只需装一个 CLI + 一条 sshd 授权，无需平台侧连接生命周期。

显式排除的错误做法：把 vsh 脚本翻译成远端 shell 命令经 `ssh host 'cmd'` 执行——会丢失 vsh 内建命令集（sed/grep/jq/awk/html-to-markdown）、grant、bg、取消语义与沙箱派生，AI 看到的不等价。

v1 非目标：RTC/UI 直连（hosts_rtc、browser/CUA 实时画面、execCall）、级联（附属的附属）、主设备向附属的自动部署（scp 引导，v2）、分块放大（v1.1）。

## 2. 拓扑与术语

```
AI/平台 ──NATS(subject=主设备路由, 信封 host_id=A1)──> 主设备 A0（验签 → 白名单 → 中继）
                                                          │ 持久 SSH 连接（stdio 帧复用）
                                                          ▼
                                                    附属设备 A1：`aic slave`
                                                    （同一 dispatch/hostfs/vsh/vbox/规则）
```

| 术语 | 含义 |
| --- | --- |
| master / 主设备 | 持有平台凭证、连接 NATS 的普通设备，同时为中继入口 |
| attached / 附属设备 | aic_hosts 中 `parent_id` 非空的行，无 secret，不连 NATS |
| slave | 附属设备上 `aic slave` 进程模式，从 stdio 读帧进 dispatch |
| relay | 主设备侧的转发面（白名单检查 + 帧复用 + attach 连接管理） |

约束：一层拓扑（parent 必须是无 parent 的真实设备）；附属行与 parent 行必须同 owner；每个 master 附属上限 16 个。

## 3. 身份与路由（平台侧）

### 3.1 数据模型

aic_hosts 加 `parent_id`（`size:32;index`，NULL = 真实设备）。附属行特征：

- `SecretEncrypted = ''`、`CredentialVer = 0`：永不发放 secret，天然无法连 NATS（`DeriveKeys` 对空 secret 报错，fail-closed）；
- `Type/Version/Hostname/DeviceInfo/Caps` 由 slave hello → 主设备 presence 上报回填（§9）；
- 在线状态沿用 LastSeen 新鲜度（心跳 20s、阈值 40s），由主设备心跳携带的 attached 状态续期（§9），无连接槽位；
- 创建走 hosts API（owner 在设备页「添加附属设备」，指定 parent，无密钥发放流程）；删除 parent 级联删除其附属行（路由依赖 parent，孤儿行无意义）。

### 3.2 路由分支（SendDeviceRequest 单漏斗）

所有出向流量（AI 工具、owner fs proxy、cancel 补发）已汇聚于 `SendDeviceRequest` 一个函数，路由分支只加在这里：

```
1. 查目标行（id=? AND owner_id=? AND status='enabled'，Select 增加 parent_id）
2. signHost = hostID
   若 parent_id 非空：
     a. 查 parent 行：owner_id=uid 且 status='enabled'，否则 "device unavailable"
     b. parent 必须在线（LastSeen 新鲜度），否则立即报错 "relay parent offline"（fail fast，不等 NATS 超时）
     c. signHost = parent_id
3. subject = NatsSubject(uid, signHost)          // u.{uid}.h.host_{signHost}.tools.req
4. key     = GetKToolCached(signHost)            // 永远用签名设备的 K_tool
5. envelope = NatsRequest{HostID: hostID,        // 目标设备 id（附属 id 时 ≠ subject 主机段）
                          Subject: subject, Caller: uid, ...}
```

签名同时覆盖 `HostID` 与 `Subject`（hosts_nats/3 现有行为），主设备无法篡改转发目标。非附属路径 signHost==hostID，现有行为逐字节不变。

### 3.3 响应与错误

slave 的 `protocol.Response` 原样经 relay → NATS reply 回到平台；平台侧现有响应身份检查（protocol 串 + request_id）不变。附属不可达/SSH 断开时，主设备对 in-flight 请求立即返回 transport Fault（`unavailable`："attached device disconnected"），不让平台等满超时。

## 4. 验签变更（protocol，关键安全面）

现状 `NatsVerify` 的硬约束：`r.HostID != host` 即拒（host 为 subject 目的地=设备自身）。附属流量信封 HostID=A1、subject 路由=A0，必然触发该约束。

新增 `NatsVerifyRelay`，`NatsVerify` 保持签名与语义不变（委托实现）：

```
NatsVerify(key, host, subject, r, now)                    // 现状：HostID 必须 == host
NatsVerifyRelay(key, host, attached func(string) bool, subject, r, now)
    // 放行条件：r.HostID == host，或 attached(r.HostID) 为真
    // 其余检查逐项不变：subject==NatsSubject(r.Caller, host)、身份字段、
    // 时效窗口（AuthorizationUntil/Deadline）、HMAC 签名、request.Validate()
```

`attached` 谓词来自主设备本地配置（§5.2 的 attach 表），**不来自信封或网络**。HandleNATS 流程变为：验签（Relay 版）→ nonce 去重（现有 replay cache，SSH 重连不影响）→ `r.HostID == 自己` 走 dispatch；否则走 relay（§5.3）。防混淆代理双闸门：平台 ownership 检查（§3.2）+ 主设备白名单。

必须配套 sign 测试向量：合法中继 / HostID 不在白名单 / 篡改 HostID 后签名失效 / caller≠uid 四类。

## 5. 中继通道

### 5.1 SSH 调用

复用 managed-ssh 的系统 OpenSSH 调用方式与固定选项集（基础设施通道，**不走 vsh `ssh` 命令、不占 ssh 域 grant**）：BatchMode、IdentitiesOnly、StrictHostKeyChecking=yes、禁转发、无 PTY、stdio 原始字节透传（Windows 经 vbox RawOutput）、ConnectTimeout=10s；新增 ServerAliveInterval=15 / ServerAliveCountMax=2 用于半开检测。远端命令固定 `aic slave`。known_hosts 引导：owner 首次手动 ssh 一次或预置（与 managed-ssh 同一约定）；建议 sshd 侧为接入钥配 ForceCommand=`aic slave` + 专用密钥。

### 5.2 attach 管理器（主设备）

cfg 新增（与 `mcp` 同级、同信任级别——设备所有者本地固定启动信息）：

```yaml
attach:
  - id: a1...            # 平台侧附属行 id（32-hex）
    target: user@host:22
    identity: ~/.ssh/aic_relay_ed25519   # 可选，缺省默认钥
```

管理器为每个表项维护一条持久 SSH 连接：断线指数退避重连（1s→30s 封顶）、连接状态机（connecting/ready/error）、向 presence 上报（§9）。配置修改重启生效（与 mcp 配置同一约定）。同一 id 重复配置、id 格式非法 → 启动校验报错。

### 5.3 relay 转发

HandleNATS 验签通过且 `r.HostID ∈ attach 表` 时：

1. 构造 caller 事实：`{subject: r.Caller, origin: r.Origin, scope: r.Scope, grant_approved: r.GrantApproved, deadline_remaining_ms: r.Deadline - platformNow, authz_remaining_ms: r.AuthorizationUntil - platformNow}`（platformNow 用主设备校准时钟；传**相对量**，slave 时钟不可信）；
2. 与 `r.Request` 打成 request 帧发进对应该 id 的 SSH 连接（连接非 ready → 立即 Fault `unavailable`）；
3. request_id 即平台信封的 Request.ID（全局唯一），连接级 in-flight 表登记，response 帧按 id 匹配后写回 NATS reply；
4. 连接断开：全部 in-flight 立即 Fault；**不重放**（平台已收错误，重试由 AI 决定）；执行侧语义见 §8。

## 6. 帧协议（hosts_relay/1）

协议类型放 protocol 包（两个 aic-pod 二进制间的线上契约，Relay* 前缀）。

### 6.1 帧格式

```
frame = uint32be(N) + N 字节 JSON（UTF-8）
N ≤ MaxFrameBytes = 1 MiB   // v1 硬上限；超限/畸形 → 关闭连接（fail-closed）
```

写路径单互斥、以帧为粒度交织：大文件分块（v1 每帧 ≤ ~34 KiB：24 KiB base64 + JSON 壳）不会堵住 cancel 帧超过一帧时间。每连接 in-flight 上限 64。读路径设空闲超时（无帧 90s 判半开，叠加 SSH ServerAlive 双保险）。

### 6.2 消息类型（`type` 字段）

| type | 方向 | 载荷 | 说明 |
| --- | --- | --- | --- |
| `hello` | slave→master | protocol、agent_version、device_type、hostname、device_info{os,arch,num_cpu}、caps（protocol.Caps）、max_chunk_bytes | 首帧；master 校 `MajorVersionMatch(self, slave)`，不符回 hello_ack{error} 并关闭 |
| `hello_ack` | master→slave | protocol、agent_version、max_chunk_bytes（取小值）、error? | 之后进入 request/response 阶段 |
| `request` | master→slave | id、caller_facts（§5.3）、request（protocol.Request） | cancel 是普通 request（action=cancel），无特殊帧 |
| `response` | slave→master | id、response（protocol.Response） | Fault 与 Result 可同时非空（现有语义） |
| `ping`/`pong` | 双向 | sent_at | 30s 应用层保活 |

未知类型忽略（前向兼容）。`max_chunk_bytes` v1 恒 24 KiB（hostfs RangeBytes 现状，该值本是为塞进 64 KiB RTC 消息定的）；v1.1 由 slave 上报 256 KiB 启用放大——SSH 通道无 RTC 尺寸约束，10 MB 文件分块往返从 430 次降到 40 次，跨 WAN 主从时收益显著（分块请求是串行 await，每往返 = NATS RTT + SSH RTT）。

## 7. slave 模式

`aic slave`：从 stdin 读帧、stdout 写帧（stderr 只写本地日志，永不出帧）。收到 request → 构造 caller → 调现有 `dispatch()`，与 RTC 入口（HandleTool）形状一致：

```go
caller := protocol.Caller{
    Subject: facts.Subject, ConnectionID: "relay:" + attachID,
    Origin: facts.Origin, Scope: facts.Scope,          // scope=fs 只读文件代理语义沿用
    GrantApproved: facts.GrantApproved,                // 审批事实透传，dispatch 内 nosandbox 检查不变
    ExpiresAt: time.Now().Add(facts.AuthzRemaining),   // Direct=false（仅 RTC 适配器置位）
}
ctx, cancel := context.WithDeadline(ctx, time.Now().Add(facts.DeadlineRemaining))
```

复用矩阵（零改动）：

| 机制 | 复用情况 |
| --- | --- |
| vsh 引擎 + 内建命令 + jq/awk | 原样 |
| vbox OS 沙箱（seatbelt/bwrap/受限令牌+ACL） | 原样，在附属设备本机 OS 落地 |
| fs 规则 / permissionState / hostfs / 24 KiB 分块 | 原样，读附属设备自己的 cfg |
| grant / grant_approved / nosandbox 审批 | 审批事实随 caller_facts 透传，dispatch 同一检查 |
| cancel / bg / 唯一运行记录表 | 原样（CancelByRequest 按 caller.Subject+Origin 归属） |
| 执行日志（.exec/*.log）、session workdir | 落在附属设备本地（cfg work_dir），AI 经 fs 工具读回 |
| `mcp.<alias>` | 附属设备本地 MCP 服务白拿（走 exec 通道）；UI 实时画面类能力除外（§12） |

slave 读本地 cfg 的授权规则/work_dir/mcp 段；`host`/`key` 段忽略。**不连 NATS、不做时钟校准**——slave 不验签，deadline 全用相对 TTL，clock.go 那套整个跳过。cfg 授权配置损坏 → permissionState 带 invalid → dispatch 现有 fail-closed 拒绝一切请求。

## 8. 生命周期语义

- **SSH 断连 = 传输断线，对齐 §2.6：不杀执行。** 前台执行继续到完成或 deadline，日志在附属设备本地落盘；重连后 AI 用 fs 读日志或显式 cancel。这与 NATS 路径「传输断线不取消执行、调用方显式 cancel 才终止」完全一致（relay 是 NATS 路径的内部延伸，不是 RTC 那种 caller 直连通道，故不采用 DisconnectTools 取消前台的语义）。
- in-flight 请求由 relay 立即 Fault `unavailable`（平台侧表现同「device request failed (effects may have occurred)」，效果可能已发生，AI 知情重试）。
- bg 任务天然不受影响（独立墙钟，任务表本地）。
- slave 进程退出/被杀：vbox 现有进程托管回收该进程组的前台执行；bg 随进程树回收规则同现状。
- master 重启：全部附属判离线（下轮 presence 不再携带），重连后自动恢复。

## 9. presence / caps 上报与防伪造

主设备 JWT 只能发自己的 subject，附属信息全部骑在主设备自己的载荷里。`HostPresence` 扩展：

```json
{ "host_id": "A0", "credential_ver": 2, "running": 1, "sent_at": "...",
  "attached": [ { "id": "A1", "state": "ready|connecting|error",
                  "agent_version": "v0.8.5", "device_type": "cli",
                  "hostname": "...", "device_info": {...}, "caps": {...} } ] }
```

平台 handlePresence 对每个 attached 项（state=ready）：

```sql
UPDATE aic_hosts SET last_seen=?, version=?, hostname=?, device_info=?, caps=?
 WHERE id=? AND parent_id=?   -- 发布者
   AND owner_id=?             -- 发布者的 owner
```

**0 行命中即丢弃并记告警——parent 绑定校验是防伪造闸门**：没有它，任一在线设备可以把别人的附属行刷成永远在线。state≠ready 不 touch last_seen，40s 后自然判离线。每 20s 全量携带（16 个上限下载荷可接受，caps 结构小）；caps 以此为准，附属永不自发 caps（无 NATS）。`$SYS` disconnect 只标主设备离线，附属随心跳缺失自然过期。

IsOnline / BuildHostListMD / ResolveTarget / `/api/hosts/{id}/proxy` 不改：附属行自动出现在 AI 的 host 列表（名字建议 UI 标注「经 {parent} 中继」），`exec.1host=a1`、owner fs proxy（同走 SendDeviceRequest 漏斗）直接可用。

## 10. 平台侧改动清单

| 位置 | 改动 | 量 |
| --- | --- | --- |
| models.Host | 加 ParentID；迁移加索引 | 小 |
| hosts API | 附属行创建（校验 parent 真实/同 owner/无 parent）、级联删除、列表标注 | 小 |
| SendDeviceRequest | §3.2 路由分支（含 parent 在线 fail fast） | 小 |
| handlePresence | §9 attached 解析 + 防伪造 UPDATE | 小 |
| **secret 为空路径审计** | 见下表 | 小但要全 |

secret 为空假设的排查表（附属行必须显式拒绝或隐藏，不能假设免费）：

| 路径 | 现状假设 | 处理 |
| --- | --- | --- |
| RTC ticket 签发 | secret 派生票据密钥 | 附属行拒绝（v1 无 RTC）；caps 不含 rtc，UI 无入口 |
| 密钥重置 API / CredentialVer | 重写 SecretEncrypted | 附属行 409 拒绝 |
| natsauth issueUserJWT | 凭 secret 认证 | 天然 fail-closed（空 secret 派生报错），无需改但加测试锁定 |
| GetKToolCached(附属 id) | 派生 K_tool | 路由分支保证永不被直接调用；保留空 secret 报错作纵深 |
| 前端设备页 | 「重新生成密钥」按钮 | parent_id 非空隐藏 |

## 11. 安全模型

- **信任边界**：平台↔主设备 = hosts_nats/3 签名（现有）；主设备↔附属 = SSH 信道认证（publickey + 严格 host key）；附属信任主设备断言的 caller 事实。
- **这不是降级**：主设备持有通往附属的 SSH 私钥，本就能在附属上执行任意命令；slave 通道没有引入新的信任方。防御重点是中继正确性：白名单（主设备本地配置）+ 签名覆盖 HostID（平台）双闸门，主设备 relay 实现 bug 也不能越出 attach 表。
- **重放**：nonce 在主设备 replay cache 去重（现有），SSH 重连/重发不产生重复执行；平台不自动重试工具请求。
- **审批事实**：grant_approved 只能沿「签名信封 → caller_facts → slave caller」流动，slave 无其他入口（stdin 唯一对端是 master）；模型/脚本自设字段无法到达。
- **SSH 加固**：固定选项集 + ForceCommand 专钥建议（§5.1）；relay 通道不走 vsh ssh 域、不占用户 grant，也不被授予 `mcp.<alias>` 之外的新能力。
- **slave 入侵面**：slave 以登录用户权限运行，执行隔离 = 本机 vbox 规则派生，与「直接 SSH 登录该机」能力完全一致；无凭证可偷（无 secret、无 NATS JWT）。
- **已知残余风险**（接受）：主设备被攻陷 → 附属连带沦陷（与持有 SSH 私钥的既有事实一致）；可选增强是给附属也发工具密钥做端到端验签（主设备退化为哑中继），收益仅防 relay 实现 bug，成本是多一套发放/吊销流程——v1 不做。

## 12. 边界与限制

- **RTC/UI 直连不可用**：附属无 secret、无 NATS，浏览器无法直连（hosts_rtc、execCall、browser/CUA 实时画面通道）。v1 支持：AI 路径（exec/fs/cancel/grant/mcp 调用）+ owner HTTP fs proxy。v2 可做主设备终结 RTC 并中继 DataChannel 帧。
- **级联禁止**：parent 必须 parent_id 为空且持 secret；创建时平台校验。
- **前置条件**：附属设备有 sshd、主→附网络可达、预装同主版本 aic-pod CLI；owner 完成 known_hosts 引导。
- **版本门禁**：hello 交换 agent_version，MajorVersionMatch 不符即拒绝（连接期暴露，不运行期诡谲）。
- **限额**：每 master ≤ 16 附属；每连接 in-flight ≤ 64；帧 ≤ 1 MiB。

## 13. 测试计划

- protocol：帧编解码 golden 向量（含边界：0 长度、恰好 1 MiB、超长拒绝）；NatsVerifyRelay 四类向量（§4）；hello 版本门禁。
- pod：slave 入口经 pipe 喂 request → dispatch 等价性（复用 unified_test 的 caller 断言：scope=fs 拒 exec、nosandbox 无审批拒、cancel 归属）；relay 集成（本机 sshd）：exec/fs/cancel/审批拒绝/断连 in-flight 立即 Fault。
- aic：路由分支矩阵（普通设备/附属/parent 离线/跨 owner/禁用行）；presence 防伪造（错 parent、错 owner、state≠ready）；附属行创建约束。
- 故障注入：SSH 中途断（执行继续 + 重连读日志）、slave 崩溃重启、重复 nonce、畸形帧关连接、半开连接（ServerAlive + 读超时）。

## 14. 实施拆解

1. **protocol**（小，安全关键）：Relay* 类型 + 帧编解码；NatsVerifyRelay + 向量；HostPresence.attached（与 aic 共用结构定义处同步）。
2. **aic**（小）：§10 清单五项。
3. **aic-pod**（中）：`aic slave` 入口（stdio 循环 → caller → dispatch，小）；relay 转发面（白名单 + facts 构造 + in-flight 表，中）；attach 管理器（连接状态机 + 重连 + presence 上报 + cfg 段，中——最大工程量在生命周期而非协议）。
4. 联调：本机双进程（master + slave over 本机 sshd）→ 跨机（mbp + linux 主机）→ AI 端到端（host list 双设备、exec/fs/grant/bg/cancel 全动作过一遍）。

## 15. 开放问题

- 断连语义已定为对齐 §2.6（不杀执行，§8）；若评审倾向 RTC 的 DisconnectTools 语义（断连取消前台），relay 策略一处可调——默认保持现状， flaky 网络下保工作成果优先。
- v1.1：chunk 放大（hello 字段已预留）；v2：主设备 scp 引导部署（VSCode Remote 式）、RTC/DataChannel 中继、附属端到端验签（§11 残余风险）。
- 多主设备共享同一附属：禁止（parent 唯一）；如未来需要，走「附属行换绑 parent」而非多 parent。
