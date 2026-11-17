# 客户端桥与设备自动接入统一设计

> 2026-11-17 整篇重写。阶段 1（服务端）、阶段 2（Go Pod）、阶段 3（aic 客户端适配）、阶段 4（Desktop）与阶段 5 第一版（iOS / Android）已实施；阶段 6（其余原生能力）仍为待实施设计。
> 覆盖 Desktop / iOS / Android 的统一宿主桥、原生登录、Web 会话接入 `/api/nc`，以及 CLI / Docker 继续使用的静态设备凭证。

## 一、目标与最终原则

### 1.1 用户体验目标

1. Desktop / iOS / Android 使用原生登录页；未登录时不再把用户送进 Web `/login`。
2. 客户端登录成功后自动完成 Host 解析或创建并上线，用户不再需要：
   - 打开平台设备页；
   - 手工创建设备；
   - 创建或复制静态密钥；
   - 回到客户端粘贴并绑定。
3. CLI / Docker 保留静态设备凭证机制，继续服务无人值守和长期在线场景。
4. aic 网页只认一个 App 能力桥 `window.aicHost`，不直接操作 Electron IPC、`WKWebView` message handler 或 Android JavascriptInterface。
5. 测试版不保留旧桥协议、旧登录路径或旧凭证格式的兼容层。

### 1.2 Host ID 与 AI 短引用

**Host 永远以完整 `host.id` 为唯一身份；AI 只使用该 ID 在当前用户空间中的 4 位短引用。**

完整 ID 固定为 36 位：

```text
host.id = <32 位 user_id><4 位 Host 序号>

示例：
user_id = 0123456789abcdef0123456789abcdef
host.id = 0123456789abcdef0123456789abcdef0007
AI ref  = 0007
```

其中：

- `user_id` 是 vbase / vigo 已生成的 32 位无连字符 UUID；创建 Host 时校验其长度和格式，非 32 位 ID 不进入拼接。
- 后 4 位是该用户空间内单调递增、补零到 4 位的 `host_seq`：`0001`…`9999`。
- 完整 36 位 ID 用于数据库、客户端连接档案、NATS、K_tool、RTC、权限和真实文件路由。
- 4 位短引用只用于 AI 上下文和 AI 工具输入输出；系统根据当前会话可信 `uid` 还原完整 ID。
- 不同账户可以都出现 `0001`，因为真实 ID 的用户前缀不同；还原时使用会话 owner uid，不能使用 viewer uid 或模型传入的 uid。
- 短引用不是第二套 Host 身份，只是完整 `host.id` 在 AI 边界的投影。

序号规则：

- Host 增加 `host_seq`，完整 ID 在创建时由 `owner_id + host_seq` 组成。
- `(owner_id, host_seq)` 唯一；事务分配加唯一冲突重试，不使用数据库全局自增主键或方言 sequence。
- 序号只增不复用，软删除记录也占位，避免旧会话或旧链接指向后来创建的设备。
- `session` 与 `device_key` Host 共用同一套 ID 规则。
- 超过 `9999` 明确报错；当前在线 Host 数量上限仍由 `MaxHostsPerUser` 控制。
- `device_uuid` 不参与序号计算，也不参与完整 ID 生成。

历史设备按本次升级统一删除，不接受旧 32 位 Host ID，不做旧 ID 别名或迁移映射。

### 1.3 `device_uuid` 只是创建前参考

`device_uuid` 只用于客户端尚无 `host.id` 时的创建前参考：

- 根据当前 Web 登录用户和 `device_uuid`，查找该账户已经登记过的 Host；
- 找到时返回已有的 `host.id`；
- 找不到时创建一个新的 Host，并返回新生成的 `host.id`；
- 客户端取得 `host.id` 后保存它，后续连接只使用 `host.id`。

`device_uuid` 不参与：

- NATS subject；
- Host 主键或对外资源 ID；
- 连接级权限判定；
- 工具请求寻址；
- caps / presence / 在线状态；
- K_tool 派生；
- RTC 票据；
- 跨账户设备关联。

Host 可以保存 `device_uuid` 作为创建参考元数据，以便客户端本地 `host.id` 丢失时重新解析；即使保存，它也不成为第二套运行时身份。

### 1.4 多账户与同一硬件 UUID

同一物理设备被多个账户轮流使用时，每个账户拥有自己的 Host：

```text
账户 A + 硬件 UUID X
  → Host A：host.id = <uidA>0001，A 的 AI 只看到 0001

账户 B + 同一个硬件 UUID X
  → Host B：host.id = <uidB>0001，B 的 AI 也只看到 0001
```

两个完整 `host.id` 因用户前缀不同而必然不同，短引用则允许相同。两行 Host 的归属、密钥、在线状态、caps、禁用和删除完全独立。

不设计：

- 全局 Device 表；
- 全局唯一硬件 UUID；
- `current_user_id`；
- 跨账户设备转移；
- 跨账户共享 Host；
- 跨账户连接互斥。

唯一查询和创建范围始终是“当前 token 用户 + device_uuid”，但最终返回并使用的是该账户自己的 `host.id`。

## 二、认证材料与职责分离

新流程使用四类材料，各自承担不同职责，不能混用。

| 材料 | 生命周期 | 用途 | 是否可暴露给 Web JS |
|---|---|---|---|
| Web access token | 短期，可刷新 | 当前用户身份；ensure 与 `/api/nc` 会话鉴权 | 否，由原生壳持有 |
| Web refresh token | 长期，单次轮换 | 换发 access token | 否，绝不进入网页、NATS 或日志 |
| `host.id` | Host 存续期稳定 | 36 位完整 ID：Host 唯一身份、subject、工具与 RTC 寻址 | 可以，不是秘密 |
| Host protocol secret | 与 Host 凭证代次一致 | 派生 K_tool、校验 RTC 票据 | 否，仅原生 Pod 与服务端持有 |

`device_uuid` 不在上表内，因为它不是认证材料，也不是 Host 身份；它只是首次解析 Host 时的参考值。

### 2.1 为什么仍需要 Host protocol secret

Web access token 只解决“当前连接代表哪个用户”，不能替代现有设备凭证中的协议秘密：

- `K_tool` 用于证明工具请求来自平台，而不是能向 NATS 发布消息的网页端；
- RTC 直连票据需要设备与平台共同持有的签名根；
- access token 会刷新和过期，不适合作为稳定 HKDF 根；
- UUID 和 `host.id` 都是标识符，不能当秘密。

因此，Web 会话模式取消的是 **vbase `aic:device` 静态密钥及用户手工绑定步骤**，不是取消工具链需要的协议秘密。

Session Host 的 protocol secret 由服务端在创建 Host 时生成并加密保存，在自动 ensure 时通过 HTTPS 返回给原生客户端。该秘密按 Host 独立，不跨账户共享，也不随 Web token 刷新而改变。

### 2.2 两类 Host 认证模式

Host 增加明确的认证模式：

```text
auth_mode = "session" | "device_key"
```

- `session`：Desktop / 移动端使用；Web access token 鉴权，Host 自己保存 protocol secret。
- `device_key`：CLI / Docker 使用；继续引用 vbase `aic:device` 静态密钥，现有复合凭证及轮换语义不变。

两种模式最终都解析出同一组运行时上下文：

```text
host.id + owner uid + credential_ver + protocol_secret
```

后续 NATS subject、K_tool 和 RTC 不需要知道用户最初通过哪种方式登录。

## 三、Session Host 的自动解析与创建

### 3.1 客户端保存的连接档案

原生客户端按“服务器地址 + 当前账户”保存：

```json
{
  "host_id": "<32位user_id><4位host_seq>，共36位",
  "device_uuid": "创建时使用的硬件参考值",
  "credential_ver": 1,
  "protocol_secret": "仅保存在原生安全存储中"
}
```

- 有完整档案：直接连接 `/api/nc`，不需要再次 ensure。
- 没有档案、档案不完整或本地需要恢复：先执行一次自动 ensure。
- `device_uuid` 不写入 NATS 连接档案的运行时身份，只保留作恢复参考。
- 切换账户时使用新账户自己的连接档案；不能复用另一个账户的 `host.id` 或 protocol secret。

### 3.2 自动 ensure 接口

新增：

```http
POST /api/nc/ensure
Authorization: Bearer <web_access_token>
Content-Type: application/json
Cache-Control: no-store

{
  "device_uuid": "客户端硬件参考 UUID",
  "device_name": "veypi-mbp",
  "device_type": "desktop"
}
```

该接口是客户端连接流程的内部引导接口，不需要用户打开设备页。

处理顺序：

1. 校验 Web access token，取得可信 `uid`；只接受动态、`inherit` 的完整登录凭证。
2. 必须存在 HTTP `Authorization: Bearer`；只有 Cookie 的请求不返回 protocol secret，避免网页仅凭 HttpOnly Cookie 读取设备签名根。
3. 在当前 `uid` 下按 `device_uuid` 查找 Host。
4. 找到 `session` Host：返回它现有的 `host.id`、代次和 protocol secret。
5. 找到 `device_key` Host：不隐式转换认证模式，返回明确冲突；转换只能由设备管理操作显式执行。
6. 没找到：创建新的 `session` Host：
   - 服务端分配当前用户的下一个 `host_seq`，组成 36 位 `host.id`；
   - `owner_id = uid`；
   - `auth_mode = session`；
   - 保存本次 `device_uuid` 作为创建参考元数据；
   - 生成随机 protocol secret 并加密保存；
   - `credential_ver` 从 1 开始。
7. 返回统一连接上下文：

```json
{
  "host_id": "36位完整Host ID（32位user_id + 4位序号）",
  "host_ref": "0001",
  "uid": "当前用户",
  "credential_ver": 1,
  "protocol_secret": "base64url 编码的随机秘密"
}
```

响应必须 `Cache-Control: no-store`，不得写访问日志或错误日志。

数据库可以为 `(owner_id, device_uuid)` 建辅助唯一索引，防止并发 ensure 重复建行；**该索引只是创建期幂等保护，不是 Host 身份，也不参与后续连接。** 静态 Host 的 `device_uuid` 使用 `NULL`，避免多个空值在三种数据库上触发唯一冲突。

### 3.3 ensure 的边界

- 同一账户、同一参考 UUID 再次 ensure：返回同一个 `host.id`，不重复创建。
- 不同账户、相同参考 UUID：各自查找或创建，返回不同 `host.id`。
- 硬件变化导致 UUID 改变：按当前账户的新参考值创建新 Host，旧 Host 由用户在设备页清理。
- 删除只影响当前账户的 Host，不影响另一个账户使用相同 UUID 的 Host。
- 禁用 Host 后 ensure 和连接都拒绝；不得借 ensure 自动恢复启用状态。
- 自动创建仍受 `MaxHostsPerUser` 限制。
- `device_name` / `device_type` 只是展示元数据，最终设备信息仍以连接后的 caps 为准。

## 四、Web 会话直连 `/api/nc`

### 4.1 CONNECT token

Session 模式的 NATS CONNECT token 定义为：

```text
s1.<host_id>.<credential_ver>.<web_access_token>
```

解析按前三个分隔点切分，最后一段保留完整三段 JWT。

其中：

- `host_id` 是来自 ensure 或本地连接档案的 36 位完整 ID，不是 AI 使用的 4 位短引用；
- `credential_ver` 是 Host protocol secret 的代次，不是 Web token 的过期时间；
- `web_access_token` 每次连接或重连时从原生认证管理器动态取得；
- protocol secret 不放进 CONNECT token，继续只用于本地 K_tool / RTC 派生；
- token 只能经 WSS 发送（纯 HTTP 开发环境允许 WS），不得放 URL query、NATS name 或日志。

用于普通 HTTP API / 技能服务时，Pod 发送原始 Web access token 作为 Bearer，不发送上述 `s1.` 包装。

### 4.2 服务端认证顺序

`natsauth` 新增 `authorizeSessionHost`：

1. 解析 `s1.` envelope，校验字段形态和最大长度。
2. 验证 Web access token 的签名、有效期和数据库状态。
3. 要求 token 为 `type=access`、动态生命周期、`mod=inherit`。
4. 从 token 得到可信 `uid`，不能相信客户端另传的 uid。
5. 按 `host.id` 查询 Host，并验证：
   - Host 存在；
   - `host.owner_id == uid`；
   - `host.auth_mode == session`；
   - Host 未禁用；
   - 请求代次等于 Host 当前 protocol secret 代次。
6. 沿用现有 `HostInboxSubject(uid, host.id)` 同 Host 互斥探测。
7. 使用现有 `issueUserJWT(uid, host.id, credential_ver)` 签发 Host 专用 NATS JWT。
8. 不创建、不刷新 vbase `aic:device` 静态密钥。

如果客户端尚未 ensure、没有有效 `host.id`，不能直接猜一个 Host 连接；客户端连接层先自动执行第三节的 ensure，再进入本流程。对用户而言仍是一次登录自动上线。

### 4.3 重连与 token 刷新

Pod 不再在启动时把 token 固化到闭包：

```text
TokenHandler
  → 读取当前账户连接档案
  → 从原生认证管理器取最新 access token
  → 组装 s1.<host_id>.<credential_ver>.<access_token>
```

规则：

- access token 刷新但 `uid`、`host.id` 未变：只更新 token provider，不重建 Host，不更换 protocol secret。
- 同一账户重新登录导致 Web key ID 改变：使用新 token 重连，Host 和 protocol secret 仍可保持不变。
- `uid` 改变：属于账户切换，必须完全断开并重建 Pod 的账户绑定状态，不能在旧连接上悄悄换 token。
- `credential_ver` 或 protocol secret 失效：停止当前连接并重新 ensure，获取该 Host 最新连接上下文。
- refresh token 只能由原生认证管理器调用刷新接口，绝不能进入 NATS。

### 4.4 静态设备模式保持不变

CLI / Docker 继续使用：

```text
配置：<36位host_id>.<vbase 四段 device credential>
NATS CONNECT：四段 credential 的前三段 static JWT
HTTP Bearer：同一个 static JWT
```

静态模式的创建、撤销、reset、最大寿命、`fs` / `skill` scope 和同名互斥逻辑不因 session 模式改变。

## 五、服务端与 Pod 的结构改造

### 5.1 Host 模型

在现有 Host 上增加或明确以下字段：

```text
id                         36位完整Host ID：32位owner user_id + 4位host_seq
owner_id                   Host 所属账户，必须等于 id 的前32位
host_seq                   当前用户空间内单调递增序号，范围1…9999
auth_mode                  session | device_key
device_uuid                可空的创建参考元数据，不参与运行时身份
device_key_id              device_key 模式引用 vbase auth_keys.id
session_secret_encrypted   session 模式的 protocol secret，加密保存
session_credential_ver     session 模式的秘密代次
credential_ver             最近一次合规 caps 的代次展示缓存，沿用现有语义
```

约束：

- `session`：`device_key_id` 为空，session secret 和代次必须有效。
- `device_key`：沿用 `device_key_id`，session 专用字段为空。
- 不允许通过 ensure 或 NATS 连接隐式切换 `auth_mode`。
- 不新增 `current_user_id`，不把 `owner_id` 改成“当前登录用户”。
- 不用 `device_uuid` 替代 `host.id`，也不在 subject 中增加 UUID 段。

### 5.2 统一 protocol secret 解析

把现有只认静态设备密钥的 `DeviceKeySecret` 收敛为按 Host 认证模式分派的内部解析器：

```go
HostProtocolSecret(ctx, hostID) -> (secret, credentialVer, error)
```

- `device_key`：继续通过 `device_key_id → vbase auth_keys.client_secret` 读取。
- `session`：读取并解密 Host 自己的 session secret，返回 session 代次。
- 字段缺失、模式不明、代次无效均 fail closed，不能因为没有 `device_key_id` 就跳过校验。

以下调用点统一改用该解析器：

- `libs/host/request.go` 的 K_tool 获取与缓存；
- `api/hosts/direct_ticket.go` 的 RTC 票据签发；
- 其他需要 Host 签名根的内部调用。

K_tool 的 HKDF 公式不变，RTC 票据格式不变，变化只在秘密来源。

### 5.3 caps、presence 与代次守卫

`Registry.credentialMatches` 改成显式按模式判断：

- `device_key`：事实源仍是 vbase `auth_keys.version`。
- `session`：事实源是 `session_credential_ver`。
- 不再保留“Host 没绑设备密钥就返回 true”的兜底。

NATS JWT 的 allow subject、caps、presence、RTC 出向 subject 全部继续使用：

```text
u.{uid}.h.{host.id}.{credential_ver}....
```

这里永远只有 36 位完整 `host.id`，没有 `device_uuid`，也没有 AI 的 4 位短引用。

### 5.4 Pod 配置拆分

当前 `Options.Key` 同时承担 Host 身份、NATS 鉴权和 HKDF 根，不适合 session 模式。拆成明确的连接配置：

```text
静态模式：
  DeviceCredential  现有复合凭证

会话模式：
  HostID
  UID
  CredentialVer
  ProtocolSecret
  AccessTokenProvider
```

两种模式在初始化后都得到相同的 `hostID / uid / credVer / kTool / HTTP Bearer provider`。

账户切换时必须重建 `uid`、`hostID`、NATS inbox、RTC、会话级 grant、后台任务和 HTTP Bearer provider，不能在旧连接上只换 token。

### 5.5 AI 短 ID 与真实执行边界

统一约定：

| 场景 | 使用的 ID |
|---|---|
| 数据库 Host.ID、owner / key 关联 | 36 位完整 ID |
| 原生客户端连接档案、`s1.` CONNECT、静态复合凭证 | 36 位完整 ID |
| NATS subject、K_tool salt、RTC 票据 | 36 位完整 ID |
| 前端真实 `/fs` 路由、持久化消息和文件引用 | 36 位完整 ID |
| AI 的 host_list、工具说明、工具输入输出、历史上下文 | 4 位短引用 |

#### AI 输入 → 完整 ID

1. `BuildHostListMD` 和启动提醒中的物理 Host `id` 只输出后 4 位，例如 `0007`。
2. `cloud` / `page` 保留字不变；物理 Host 的 `_host` 使用 4 位短引用。
3. `ResolveToolTarget` 从当前 `types.Session.UserID()` 取得可信 uid：
   - 对 4 位数字目标组成 `uid + target`；
   - 在当前用户的 Host 列表中验证存在、启用和归属；
   - 将工具输入 `_host` 重写为 36 位完整 ID；
   - 后续 `SendToolRequest`、NATS 和 Pod 完全不接触短引用。
4. 设备名称解析可以继续作为便利入口，但歧义候选和错误消息只展示 4 位 ID。
5. AI 不提交 uid，也不能通过传入另一个 user_id 选择别的用户空间。
6. 旧 32 位 ID 直接拒绝；36 位完整 ID 是内部协议值，不作为 AI 推荐输入。

#### 内部结果 → AI 上下文

内部执行结果可以继续保存：

```text
attrs.from = host/<36位完整ID>
/fs/<36位完整ID>/native/path
```

构造 LLM 上下文时统一投影为：

```text
attrs.from = host/0007
/fs/0007/native/path
```

投影范围包括：

- host_list 与其他系统提醒；
- fs / exec 工具描述和 schema 示例；
- tool call 输入回放；
- tool result 的 `attrs.from`、文件链接和错误文本中的结构化 Host 引用；
- 历史 assistant / tool 消息及压缩摘要中的 `/fs` Host 段。

不能做全局四位数字字符串替换；只转换明确的 `_host`、`host/{id}`、`/fs/{id}/...` 等结构化位置，普通文本中的数字保持不变。

#### AI 输出 → 持久化与前端

AI 最终回复中允许使用短链接：

```text
[report](/fs/0007/Users/veypi/report.md)
```

在消息持久化前，系统按**该消息所属 session 的 owner uid**解析为完整 36 位 Host ID并验证 Host 归属，再保存完整 `/fs` 链接。前端始终使用完整 ID 路由，不根据当前 viewer 猜短 ID 属于谁。

这样同时保证：

- AI 全程只看到 4 位；
- 数据库存储和前端链接没有用户空间歧义；
- 分享或跨账户查看消息时，仍指向原 session owner 的 Host，不会错误解析成 viewer 自己的 `0007`；
- 无法解析的短 Host 引用明确报错，不留下不可路由的模糊链接。

#### 短 ID 不是权限边界

4 位序号可枚举是可以接受的，因为 Host ID 从来不是凭证。真正的权限来自：

- 当前 session 的可信 owner uid；
- 当前用户 Host 列表与启用状态；
- `host.owner_id == session.UserID()`；
- NATS / 静态密钥 / Web session 的既有认证链。

## 六、原生认证与 WebView 登录态

### 6.1 原生壳是认证状态所有者

Desktop / iOS / Android 都由原生壳负责：

1. 展示原生登录页；
2. 调用 vbase 登录接口；
3. 安全保存 refresh token 和当前 access token；
4. 向 WebView 注入必要的 HttpOnly 会话 Cookie；
5. 向 Pod token provider 提供最新 access token；
6. 统一执行刷新、登出和账户切换。

WebView 中的 Cookie 只是网页请求所需的登录态镜像，不是第二套认证状态，也不是 Pod 的 token 来源。

原生桥 `auth_state` / `auth.changed` 只返回 `{signedIn, user, expiresAt}` 等状态，不返回 access token 或 refresh token。

### 6.2 刷新只有一个执行者

默认 access token 有效期只有 15 分钟，必须避免 WebView 与 Pod 各自刷新：

- 原生认证管理器统一 singleflight 刷新；
- Pod 只读取原生管理器提供的最新 access token；
- 原生模式下，vbase 网页侧的 `ensure()` / `checkRefresh()` 通过 `aicHost.call('auth_refresh')` 请求宿主刷新；
- 宿主刷新成功后更新 WebView Cookie，并发出 `auth.changed`；
- 纯浏览器没有 `aicHost.capabilities.auth`，继续使用现有 vbase 刷新路径。

refresh token 的轮换响应必须原子写回原生安全存储。Desktop 保留退出前 `cookies.flushStore()`，但它只是 WebView Cookie 落盘保护，不能替代原生 token 状态的持久化。

### 6.3 登出

原生模式登出顺序：

1. aic 完成服务端 logout / 当前 Web 会话撤销；
2. aic 的客户端适配层调用 `aicHost.call('auth_sign_out')`；
3. 宿主停止 Pod、断开 NATS 与 RTC；
4. 清理当前账户 token、WebView Cookie 和网页缓存登录态；
5. 回到原生登录页；
6. 网页不再自行跳 `/login`。

登出默认不删除 Host。下一次同账户登录时，通过已有连接档案或 ensure 继续使用原来的 `host.id`。

vbase 核心包不直接引用 `window.aicHost`。vbase 只提供通用登出完成钩子，aic 的客户端适配层负责调用宿主桥并控制原生模式下的跳转。

### 6.4 切换账户

切换账户不是修改某个共享 Device 的当前用户，而是切换当前客户端使用的 Host 连接档案：

1. 完整执行旧账户断开和本地会话清理；
2. 新账户原生登录；
3. 如果已有新账户的连接档案，直接使用其中的 `host.id`；
4. 否则携带同一个硬件 `device_uuid` 执行 ensure；
5. 服务端只在新账户下查找或创建 Host，返回新账户自己的 `host.id`；
6. 旧账户 Host 保持原样，不更新、不转移、不删除。

## 七、统一 `window.aicHost` 协议

### 7.1 对象与能力探测

```js
window.aicHost = {
  version: 1,
  platform: 'desktop' | 'ios' | 'android',
  capabilities: {
    auth: true,
    share: true,
    clipboard: true,
    files: true,
    notify: true,
    external: true,
    deeplink: true,
    theme: true,
    locale: true,
    accountDelete: true
  },
  call(method, params) {},       // Promise
  on(event, handler) {},         // 返回 unsubscribe
  off(event, handler) {}
}
```

- 网页只通过 `call()` / `on()` 使用原生能力。
- 无 `window.aicHost` 就是纯浏览器环境，行为保持 Web 路径。
- 功能判断必须看 `capabilities`，不能靠 UA、URL 或平台名称猜测。
- 方法名使用 `^[a-z_]+$`；所有方法异步返回 Promise，失败 reject，不静默吞错。
- 事件名使用点分层级，如 `auth.changed`。

### 7.2 页面 → 宿主方法

| method | params | result | 用途 |
|---|---|---|---|
| `app_info` | — | `{version, platform, build}` | 客户端信息 |
| `auth_state` | — | `{signedIn, user, expiresAt}` | 查询原生登录态，不含 token |
| `auth_refresh` | — | `{signedIn, user, expiresAt}` | 请求宿主统一刷新并同步 Cookie |
| `auth_sign_out` | — | `{ok}` | 登出、停止 Pod、清凭据并回原生登录页 |
| `auth_open_login` | — | `{ok}` | 网页发现未登录时请求切回原生登录页 |
| `account_delete` | — | `{ok}` | 原生确认后执行账户注销 |
| `share` | `{text?, url?, title?}` | `{ok}` | 系统分享 |
| `clipboard_write` | `{text}` | `{ok}` | 写剪贴板 |
| `clipboard_read` | — | `{text}` | 读剪贴板，按系统权限执行 |
| `files_pick` | `{accept?, multiple?}` | 文件引用或内容句柄 | 原生文件选择 |
| `files_save` | `{name, mime, source}` | `{ok, uri?}` | 导出文件 |
| `notify` | `{title, body?, id?}` | `{ok}` | 本地通知 |
| `external_open` | `{url}` | `{ok}` | 打开外部链接或其他 App |
| `deeplink_open` | `{url}` | `{ok}` | 处理 `ivec://` 深链 |
| `theme_get` / `theme_set` | `{value?}` | `{value}` | 主题 |
| `locale_get` / `locale_set` | `{value?}` | `{value}` | 语言 |
| `page_error` | `{message, stack?, url?}` | `{ok}` | 网页错误上报给宿主 |

删除原草案中的 `auth_set_token`：网页不向宿主上传长期 token，宿主也不通过桥把 token 暴露给网页。

文件接口不规定所有文件都转成 base64。小文件可直接返回内容；大文件使用原生文件引用、流式句柄或 `/fs/{host_id}` 引用，避免阻塞桥通道。真实路由使用 36 位完整 ID，AI 上下文统一投影为 `/fs/0001/...` 形式的 4 位短引用。

### 7.3 宿主 → 页面事件

| event | payload | 用途 |
|---|---|---|
| `auth.changed` | `{signedIn, user, expiresAt}` | 登录、刷新、登出、账户切换后的状态同步 |
| `server.changed` | `{server}` | 服务地址变化，网页保存状态后重载 |
| `theme.changed` | `{value}` | 系统或宿主主题变化 |
| `locale.changed` | `{value}` | 宿主语言变化 |
| `navigate` | `{path}` | 深链或通知要求网页跳转 |

`page.error` 不是宿主事件，已改为页面调用 `page_error`。

### 7.4 三端实现映射

| 能力 | Desktop Electron | iOS WKWebView | Android WebView |
|---|---|---|---|
| 桥注入 | preload `contextBridge` | `WKUserScript` + Promise 回执 | `postMessage` / JavascriptInterface |
| 登录 | 原生登录页 + 默认 session Cookie | 原生登录页 + WebView Cookie | 同 iOS |
| token 保管 | 主进程安全存储 | Keychain | Keystore |
| Pod | 本地 Go backend | 不作为常驻后台设备 | 按平台能力另行评估 |
| 文件 | Electron dialog / 原生路径 | UIDocumentPicker | Storage Access Framework |
| 分享 | 系统分享或剪贴板 | UIActivityViewController | Intent |
| 通知 | Notification | UNUserNotificationCenter | 系统通知 |

Desktop 的 Pod 自管能力与 App 能力分层：

- `window.aicHost`：三端统一的 App 能力；
- Desktop 自有管理桥：配置、日志、启停、静态 CLI 凭证管理；
- 网页普通业务不直接调用 Electron `local:api`。

## 八、三端统一的原生登录与设置

### 8.1 登录页信息架构

```text
┌─ 右上角：设置 ────────────────────────────┐
│                ivec.ai logo               │
│                  ivec.ai                  │
│               登录以继续                   │
│                                           │
│  密码登录 | 验证码登录（按后端配置显示）     │
│  用户名或邮箱                              │
│  密码 / 验证码                             │
│  忘记密码？                                │
│  同意用户协议与隐私政策                     │
│  [登录]                                   │
│  没有账号？去注册                          │
└───────────────────────────────────────────┘
```

- 登录方式由后端 `auth.login.methods` 决定。
- 用户协议和隐私政策按后端配置显示。
- 登录错误展示服务端返回原文。
- 登录成功后由原生壳建立认证状态、注入 Cookie、解析或创建 Host，再加载工作区。
- 任一自动接入步骤失败都明确报错，不出现“网页已登录但设备状态未知”的假成功。

### 8.2 设置页信息架构

| 分组 | 项 | Desktop | 移动端 |
|---|---|---|---|
| 服务 | 平台服务器地址 | 可修改，修改后重新登录并重连 | 可修改 |
| 本地 Pod | 运行状态、日志、启停 | 有，仅 Desktop | 无常驻 Pod，不展示 |
| 外观 | 主题 | 跟随系统 / 浅色 / 深色 | 同 |
| 外观 | 界面语言 | 跟随系统 / 中文 / English | 同 |
| 账户 | 当前账户 | 显示 | 显示 |
| 账户 | 注销账户 | 原生确认后调用服务端 | 同 |
| 隐私 | 用户协议 / 隐私政策 | 链接 | 链接 |
| 关于 | 版本 / 开源地址 / 许可证 | 同 | 同 |

**平台服务器地址与本地 Pod 是两个概念。** Desktop 的 Go backend 在本机，不代表平台只能连接本机；现有远程平台地址能力保留。

设置入口放在原生登录页右上角；登录后也可通过原生工作区入口打开。Desktop 复用现有 settings 窗口实现，标题统一为“设置”。

### 8.3 文案单一真源

唯一来源：

```text
aic-pod/client-bridge/l10n.json
```

- Desktop 直接读取 JSON。
- iOS 构建时生成 Swift 资源或打包 JSON 运行时读取。
- Android 从同一 JSON 生成资源。
- Web 无宿主模式继续使用 aic / vbase 自己的语言包。
- 删除 Swift `L10n.swift` 与 Desktop `langs.json` 的双重维护，不做旧文案表兼容。

## 九、其他原生能力与后台任务

| 场景 | 使用方法 | 接入位置 |
|---|---|---|
| 原生登录与刷新 | `auth_state` / `auth_refresh` / `auth.changed` | aic 认证适配层 |
| 登出回原生登录页 | `auth_sign_out` | aic 登出完成钩子 |
| 自助注销账号 | `account_delete` | 设置页 |
| 文件上传 / 导出 | `files_pick` / `files_save` | 会话输入、成果导出 |
| 分享会话成果 | `share` | 会话头部、成果卡片 |
| 复制代码或链接 | `clipboard_write` | 代码块、链接 |
| 任务完成提醒 | `notify` | 任务中心、提醒 |
| 外部链接 | `external_open` | 文档、mailto、第三方链接 |
| 深链唤起 | `deeplink_open` + `navigate` | 邮件、通知、其他 App |
| 主题语言跟随 | `theme_*` / `locale_*` | aic 设置与初始化 |
| 客户端能力差异 | `capabilities` | 功能入口显隐 |

后台任务原则：

- aic 定时任务、长任务和提醒必须以服务端调度为可靠兜底。
- Desktop Pod 常驻时可以执行设备任务，但 App 退出、系统休眠和断网都要被视为正常情况。
- iOS App 被杀或进入后台后，不承诺 NATS 长连接、定时器或本地任务继续运行。
- 移动端正持续接入的首先是 App 能力桥，不为了实现统一登录而虚构一个可靠的常驻移动 Pod。

## 十、安全边界

1. 36 位完整 `host.id` 是 Host 的唯一身份；AI 的 4 位短引用必须按 session owner uid 还原；`device_uuid` 只用于创建前解析，不是秘密、不签发权限。
2. Web access token 决定 ensure 和 session 连接的用户；Host 归属必须等于 token 用户。
3. refresh token 永不进入 Web JS、Pod 配置明文、NATS、URL 或日志。
4. ensure 只有显式 Bearer 才返回 protocol secret；Cookie-only 请求可以识别登录态，但不能取签名根。
5. protocol secret 按 Host 独立，不跨账户共享；K_tool 公式和 RTC 校验边界不变。
6. `/api/events` 的网页用户 NATS JWT 与 `/api/nc` 的 Host NATS JWT 继续分离，不能借 session 接入让网页订阅 `host_` 工具流量。
7. 同一 `host.id` 继续单连接互斥；不同账户的不同 `host.id` 即使来自相同硬件 UUID，也互不互斥。
8. 禁用、删除、reset 都只作用于当前账户的 Host。
9. 原生桥只注入受信任平台 origin；跳转外部不可信页面后撤销桥能力。
10. 错误显式返回，不用静默 fallback 把认证失败伪装成已连接。

## 十一、实施拆分

### 阶段 1：Session Host 协议与服务端

主要落点：

- `aic/models/host.go`：36 位完整 ID、每用户 `host_seq`、认证模式、创建参考元数据、session secret 与代次。
- `aic/libs/host/manager.go`：每用户序号分配、唯一冲突重试、删除后不复用，以及 session Host 创建、查询和秘密管理。
- `aic/api/nc`：新增 ensure，保留原 WebSocket 接入。
- `aic/libs/natsauth/auth_service.go`：新增 `s1.` 的 `authorizeSessionHost`。
- `aic/libs/host/device_key.go` / `request.go`：统一 protocol secret 解析。
- `aic/libs/host/registry.go`：按认证模式执行代次守卫。
- `aic/api/hosts/direct_ticket.go`：RTC 使用统一秘密解析。

完成标准：不接触客户端 UI，也能用测试完成 ensure、Web token 鉴权、caps、工具签名与 RTC 全流程。

### 阶段 2：Go Pod 会话连接配置

主要落点：

- `aic-pod/protocol`：定义 `s1.` session CONNECT envelope 与解析测试。
- `aic-pod/libs/host/client.go`：拆分静态凭证和 session 连接配置。
- 新增动态 access token provider，重连时不再复用启动时的旧 token。
- 保留静态 `Options.Key` 路径给 CLI / Docker；不为二者制造伪兼容配置。
- 增加账户切换时的完整连接重建入口。

实施落点：`protocol/host_id.go`、`protocol/session_cred.go`、`libs/host/session_config.go`；`Options.Key` 与 `Options.Session` 互斥，`StartOptions` / `SwitchAccount` 管理完整 Client 生命周期。NATS 每次连接和重连调用 provider 组装 `s1.`，普通 HTTP 每次调用同一 provider 但只发送原始 Web access token。静态凭证、Session secret、token 均不写日志。

### 阶段 3：aic 客户端适配层

主要落点：

- `aic/libs/host/resolve.go` / `target.go` / `hostlist.go`：4 位 AI 引用解析、Host 列表和错误候选。
- `aic/libs/runtime` 与消息上下文装配：tool call、tool result、历史消息和 `/fs` 链接的双向 ID 投影。
- `aic/ui/shared/client-host.js`：能力探测、`call` 封装、事件订阅和资源清理。
- aic 认证适配：原生模式调用 `auth_refresh` / `auth_sign_out`，纯 Web 保持原路径。
- vbase 只增加通用刷新与登出钩子，不 import 或硬编码 aic 产品桥。
- 文件、分享、通知等能力按 `capabilities` 逐项接入。

实施落点：`libs/host/projection.go` 统一结构化双向投影；`BuildHostListMD`、`ResolveToolTarget`、`BuildRuntimeMessages` 和 `sessionctx.finishAssistant` 只按 session owner 解析短引用，未知引用标错并阻止 tool dispatch。`ui/shared/client-host.js` 提供能力/方法白名单与事件清理；vbase 的通用 refresh/logout/login-redirect 钩子由 aic 适配，纯 Web 不变。会话附件接 `files_pick`，cloud/host 下载接 `files_save`（page OPFS 保留 Blob），消息成果接 `share`，复制按能力走原生或 Web。前端所有真实 `/fs` 入口只接受完整 36 位 Host ID。任务完成通知等待可靠终态事件与去重键，主题、语言、深链等仍属后续阶段。

### 阶段 4：Desktop

- 原生登录页和统一设置入口。
- 主进程认证管理器、token 安全存储、Cookie 同步和 singleflight 刷新。
- 首次登录自动 ensure，保存并复用 `host.id` 连接档案。
- `remote-preload.js` 暴露 `aicHost`，普通网页不直接碰 `local:api`。
- 本地 Pod 管理与 App 桥分层。
- 登出和账户切换完整停止并重建 Pod。

实施落点：`desktop/settings-ui/login.html` + `login-preload.js` 提供独立原生登录；`native-auth.cjs` / `native-app.cjs` 统一认证、ensure 与生命周期，`auth-store.cjs` 用 Electron `safeStorage` 按“服务器 + 账户”加密保存 token 和 Host 档案。Cookie 由主进程镜像到 `defaultSession` 并落盘，刷新 singleflight；公开 `auth_state` 不含 token。`aic-backend desktop-session` 使用 stdin/stdout 私有 JSON RPC：`session_bootstrap` 只下发稳定 Host 档案，`access_token` 在每次 NATS 连接/重连与 HTTP 请求时动态取回原始 Web access token。stdout 仅限协议帧，日志只走 stderr/文件，不监听本地端口，秘密不进 argv/env/config.yaml。

`remote-preload.js` 同时暴露统一 `aicHost`（当前 auth/external，其余能力 false）与 Desktop 固定管理方法；普通网页不再获得任意 `local:api` 转发或 bind/unbind。Hosts 页在原生 Session 下隐藏手工绑定面板。登出、切换账户和切换服务器完整停止 Pod，并销毁平台 SharedWorker 与桌宠；切回账户复用其 Host 档案。共享文案源已落到 `client-bridge/l10n.json` 并纳入打包检查。

验证：Desktop node 测试覆盖 Cookie-only token、刷新去重、账户变化 fail-closed、秘密落盘、stdio RPC、A→B→A 档案复用与失败不进入工作区；Go CLI 测试、`go build ./...`、aic 前端 938 项和 Hosts 页 vhtml check 通过。未做实机 Desktop/浏览器验收，实际应用需重新构建 backend 并重启 Electron。

### 阶段 5：iOS / Android

- 将现有 iOS `ShellBridge` 收敛到 `aicHost`，不保留旧消息兼容期。
- 原生认证管理器统一持有 token，解决 WebView 刷新后 Keychain 中 Cookie 过期的问题。
- 统一登录、设置、注销和错误展示。
- Android 按同一协议实现。
- 移动端后台任务以服务端调度为准。

实施落点（2026-11-17 第一版）：iOS 删除旧 `ShellBridge` 与壳内第二份文案表（`L10n.swift` 删除，文案改读 `client-bridge/l10n.json`，构建时打进 App 包）；新增 `IvecAI/Shared/AuthManager.swift`（access/refresh token 存 Keychain、刷新 singleflight、刷新后向 WebView 重新下发 cookie）、`AuthModels.swift`（凭据模型 + JWT / Set-Cookie 解析）、`AuthStore.swift`（按服务器隔离的登录档案）、`AicHostBridge.swift`（`window.aicHost`：能力探测、`call` Promise 回执、`on/off` 事件）、`AppCoordinator.swift`（桥方法接线）；工作区只留平台 origin，站外链接交给系统（原生桥只注入受信任 origin）。Android 新建 `mobile/android/`：同一套 `window.aicHost` 协议（`@JavascriptInterface` + `WebViewCompat.addDocumentStartJavaScript`）、Keystore(AES/GCM) 加密的登录档案、原生登录页与工作区 WebView 容器。能力面：当前只有 `auth` 与 `accountDelete` 为真，文件 / 分享 / 剪贴板 / 通知 / 外部链接 / 深链 / 主题 / 语言仍属阶段 6。
未做：iOS 与 Android 的协议同意页与完整设置页（iOS 已有同意页，设置页只有账户分组）；Android 未构建验证（本机无 SDK）；移动端后台任务按服务端调度为兜底，App 被杀不承诺常驻连接。

### 阶段 6：其余原生能力

按优先级接入文件、分享、剪贴板、通知、外部链接、深链、主题和语言；每项都必须有能力探测和纯 Web 降级行为。

## 十二、验收与回归用例

### 12.1 Host 解析与多账户

1. A 账户首次 ensure UUID X：创建 `<uidA>0001`。
2. A 再次 ensure UUID X：仍返回 `<uidA>0001`，Host 数量不增加。
3. B 账户 ensure 同一个 UUID X：创建或返回 `<uidB>0001`，与 A 的完整 ID 不同。
4. B 的 ensure、禁用、删除、reset 不改变 A 的 Host。
5. A 本地保存完整 36 位 ID 后，后续 `/api/nc` 不再发送 UUID。
6. 客户端丢失本地 `host.id` 后重新 ensure，可以找回 A 原来的完整 ID。
7. 并发 ensure 不重复创建 Host；达到设备上限时明确报错。
8. `device_key` Host 与 session ensure 冲突时不被隐式转换。

### 12.2 Session NATS

1. 有效 Bearer + `host.id` + 正确代次可以连接。
2. token 用户与 Host owner 不同、Host 被禁用、模式错误或代次过期均拒绝。
3. Cookie-only ensure 不返回 protocol secret。
4. access token 刷新后重连成功，`host.id`、代次和 protocol secret 不变。
5. 同一 `host.id` 双连接仍被互斥；不同账户的不同 `host.id` 不互相顶号。
6. NATS subject、caps、presence、工具请求和 RTC 中只出现 36 位完整 `host.id`，不出现 `device_uuid` 或 4 位短引用。

### 12.3 工具与 RTC

1. session Host 的 K_tool 与静态 Host 使用相同签名向量和 HKDF 公式。
2. session protocol secret 轮换后旧工具签名和旧 RTC 票据失效。
3. 静态设备 reset 的既有语义不变。
4. 缺失秘密、未知认证模式、错误代次全部 fail closed。
5. 网页前端 NATS 身份仍不能订阅物理 Host 的工具流量。

### 12.4 原生认证

1. 首次启动展示原生登录页，登录后自动上线，无手工绑定步骤。
2. WebView 与 Pod 不会并发 refresh；刷新后 Cookie 和 Pod token 同步更新。
3. 登出停止 Pod、清理 token 与 Cookie并回原生登录页，不跳 Web `/login`。
4. A 切换到 B 时，旧连接完全销毁；B 使用自己的 `host.id`，A 的 Host 数据不变。
5. 切回 A 可以复用 A 原有连接档案。
6. 强杀后重启不会因为旧 refresh token 或旧 Cookie 陷入重复撤销。
7. 纯浏览器没有 `aicHost` 时，登录、刷新、登出行为不变。

### 12.5 CLI / Docker

1. 新的 36 位静态复合凭证可以连接。
2. 静态凭证撤销、reset、过期和设备禁用行为不变。
3. CLI / Docker 不需要 WebView、原生登录页或 `aicHost`。

### 12.6 Host ID 与 AI 投影

1. 同一用户创建 Host 得到 `<uid>0001`、`<uid>0002`，序号补零且不复用。
2. 不同用户可以都有短 ID `0001`，完整 Host.ID 不同，互不影响。
3. AI 的 host_list、工具输入、工具结果和压缩后历史只看到 4 位短 ID。
4. `_host=0001` 在当前 session owner 下还原为 36 位 ID；其他用户空间不能混用。
5. Pod、NATS、K_tool、RTC 和静态凭证只收到 36 位完整 ID。
6. AI 输出 `/fs/0001/...` 在持久化前扩展为完整链接；前端真实路由不依赖 viewer uid。
7. 名称歧义、离线、未知 Host 等错误候选只向 AI 暴露 4 位 ID。
8. 普通文本中的四位数字不被替换，旧 32 位 ID 不被接受。
9. 并发创建序号不冲突，软删除后新建不复用旧序号。

## 十三、明确不做

- 不做全局 Device / 硬件 UUID 身份体系。
- 不做 `current_user_id` 或跨账户 Host 转移。
- 不让 `device_uuid` 成为 `host.id` 的别名或第二套身份。
- 不把 Web refresh token 当设备密钥。
- 不用 access token、UUID 或 `host.id` 直接派生 K_tool。
- 不让 vbase 核心依赖 `window.aicHost`。
- 不保留旧 ShellBridge、旧桌面网页登录或旧配置格式兼容层。
- 不承诺移动端 App 被杀后的常驻设备连接。
- 不兼容旧 32 位 Host ID；历史设备统一删除后重新创建。
- 不使用数据库全局自增主键，不在删除后复用 Host 序号。
- 不在本次修改版本号，不提交或推送代码。

