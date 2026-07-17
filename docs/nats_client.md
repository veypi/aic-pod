# AIC Env 客户端连入指南

## 概述

AIC Env 服务允许第三方设备（服务器、沙箱、IoT 设备）通过 NATS over WebSocket 连入 AIC 平台。连入后，设备上的命令执行、文件操作等能力会自动注册为 LLM 可调用的工具。

**默认连入地址**: `wss://ivec.ai/aic/api/nc`

客户端只需 **凭据（credential）** 即可连接，无需额外账号配置。

---

## 协议规范

以下为与语言实现无关的协议参考，适用于任何语言（Go、TypeScript、Dart 等）实现客户端。

### NATS Subject 拓扑

所有 subject 前缀为 `u.{uid}`。客户端通过 JWT 限域到自己的环境范围内。

| Subject | 方向 | 说明 |
|---------|------|------|
| `u.{uid}.e.{env_id}.{cred_ver}.caps` | Client → Server | 能力声明，连接成功后立即发布 |
| `u.{uid}.e.{env_id}.{cred_ver}.presence` | Client → Server | 心跳，每 20s |
| `u.{uid}.e.{env_id}.{cred_ver}.tool.{name}.req` | Server → Client | 工具请求（req-reply） |

### 连接鉴权

1. 客户端使用 Token 认证连接 NATS
2. Token 格式: `e1.{env_id}.{env_info_b64}.{ts_ms}.{nonce_b64}.{sig_b64}`
3. NATS Server 通过 Auth Callout 验证 HMAC 签名
4. 通过后签发限域 JWT，客户端只能访问自己环境的 subject

### CAPS — 能力声明

**Subject:** `u.{uid}.e.{env_id}.{cred_ver}.caps`

**Payload:**

```json
{
  "env_id": "abc123",
  "agent_version": "1.0.0",
  "credential_ver": 1,
  "device_type": "device",
  "device_name": "my-pc",
  "device_info": {
    "hostname": "my-pc",
    "os": "darwin",
    "arch": "arm64",
    "num_cpu": 8,
    "go_version": "go1.22"
  },
  "tools": [
    {
      "name": "exec",
      "description": "Execute a program. action is the program name, argv are its arguments.",
      "parameters": {
        "type": "object",
        "properties": {
          "action": { "type": "string" },
          "argv": { "type": "array", "items": { "type": "string" } }
        },
        "required": ["action", "argv"]
      },
      "required_level": 2,
      "policy_version": "1"
    }
  ]
}
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `tools[].name` | string | 工具名，对应 subject `tool.{name}.req` |
| `tools[].description` | string | LLM 用描述 |
| `tools[].parameters` | JSON Schema | 输入参数 schema |
| `tools[].required_level` | int | **权限等级**，见下方说明 |
| `tools[].policy_version` | string | 策略版本标识，可自定义 |

### Presence — 心跳

**Subject:** `u.{uid}.e.{env_id}.{cred_ver}.presence`

**Payload:**

```json
{
  "env_id": "abc123",
  "credential_ver": 1,
  "running": 1,
  "sent_at": "2026-07-18T10:30:00Z"
}
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `running` | int | 固定 1（表示在线） |
| `sent_at` | string | RFC3339 UTC 时间戳 |
| `credential_ver` | int | 凭据版本号 |

### Tool Request — 服务端发送工具请求

**Subject:** `u.{uid}.e.{env_id}.{cred_ver}.tool.{tool_name}.req`

**方向:** Server → Client (NATS Request-Reply)

**Payload (EnvToolRequest):**

```json
{
  "msg_id": "msg_abc123",
  "session_id": "sess_xyz",
  "tool_name": "exec",
  "tool_data": {
    "action": "ls",
    "argv": ["-la", "/tmp"]
  },
  "granted_level": 3,
  "nonce": "abc123def456",
  "deadline": "2026-07-18T10:31:00Z",
  "sig": "base64url_hmac_signature",
  "env_id": "abc123",
  "approval": {
    "fingerprint": "approve-abc",
    "resolved_by": "user_id",
    "resolved_at": "2026-07-18T10:30:30Z"
  }
}
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `msg_id` | string | 请求唯一 ID，响应时必须原样返回 |
| `session_id` | string | LLM 会话 ID |
| `tool_name` | string | 工具名，对应 CAPS 中声明的 name |
| `tool_data` | object | 工具参数。**内置工具**用 `{action, argv}` 格式；**自定义工具**自行定义 |
| `granted_level` | int | **当前授予的权限等级**，客户端据此判断是否允许执行 |
| `nonce` | string | 随机数 Base64URL |
| `deadline` | string | RFC3339 截止时间，过期请求客户端应拒绝 |
| `sig` | string | HMAC-SHA256(K_tool, canonical)，客户端**必须验签** |
| `env_id` | string | 目标环境 ID |
| `approval` | object | 审批凭证。存在表示用户已事前批准。`fingerprint` 为审批指纹，`resolved_by` 为审批人 |

### Tool Response — 客户端回复

**方向:** Client → Server (NATS Reply)

**Payload (EnvToolResponse):**

```json
{
  "msg_id": "msg_abc123",
  "status": "completed",
  "content": "file1.txt\ndir2/\n",
  "attrs": {
    "action": "ls",
    "path": "/tmp",
    "rows": "2",
    "path_kind": "mixed",
    "truncated": "false"
  }
}
```

**status 枚举：**

| Status | 含义 | content | error | need_approval |
|--------|------|---------|-------|---------------|
| `completed` | 成功完成 | 执行结果 | — | — |
| `error` | 执行失败 | 部分输出（如有） | 错误描述 | — |
| `rejected` | 拒绝执行 | — | 拒绝原因 | — |
| `waiting` | **动态审批** | 预览信息 | — | **必填** |

`attrs` 为 `map[string]string`，可选。各 action 的具体字段见[第七节工具实现](#七工具实现)。

**waiting 状态示例：**

```json
{
  "msg_id": "req_xxx",
  "status": "waiting",
  "content": "About to delete 500 records in table users",
  "need_approval": {
    "reason": "This will permanently delete 500 records",
    "wait_type": "approval",
    "preview": "DELETE FROM users WHERE ...",
    "fingerprint": "delete-users-123"
  }
}
```

| need_approval 字段 | 类型 | 说明 |
|------|------|------|
| `reason` | string | 审批原因，展示给用户 |
| `wait_type` | string | 等待类型，固定 `"approval"` |
| `preview` | string | 预览信息，帮助用户决策 |
| `fingerprint` | string | 审批指纹，服务端重试时原样带回 |

### 权限等级

每个工具声明时设置 `required_level`，服务端根据会话配置下发 `granted_level`。

| Level | 值 | 含义 | 典型场景 |
|-------|-----|------|---------|
| Confirm | 1 | 每次需用户确认 | 文件修改、数据删除 |
| Auto | 2 | 安全操作自动执行 | 文件读取、信息查询 |
| Allow | 3 | 全部自动允许 | 任意命令执行 |

**客户端校验逻辑：**

```
if granted_level >= required_level
    → 正常执行

if granted_level < required_level && approval 存在
    → 用户已事前批准，正常执行

if granted_level < required_level && approval 不存在
    → 拒绝，返回 status: "rejected"
```

### 审批流程

**事前审批（Pre-execution）：**

```
1. LLM 选择工具，服务端判断 granted_level < required_level
2. 服务端向用户发起确认请求
3. 用户确认 → 服务端重发请求，附带 approval 字段
4. 客户端收到 approval → 正常执行
```

**动态审批（Runtime waiting）：**

```
1. 客户端开始执行（事前已通过或无需审批）
2. 执行中遇到高风险操作，需要用户确认
3. 客户端返回 status: "waiting" + need_approval
4. 服务端向用户展示审批信息
5. 用户确认 → 服务端重发请求，附带 approval（含 fingerprint）
6. 客户端收到 approval → 继续执行
7. 用户拒绝 → 不再重试
```

**动态审批 handler 示例（伪代码）：**

```
function handle(data, grantedLevel, approved):
    if not approved:
        return {status: "waiting", need_approval: {reason: "...", fingerprint: "abc"}}

    // approved = true → 执行实际操作
    result = execute(data)
    return {status: "completed", content: result}
```

---

## 一、获取凭据

在 AIC 平台上创建环境后，会得到凭据，格式为：

```
<env_id>.<cred_ver>.<secret>.<uid>
```

| 字段 | 含义 | 说明 |
|------|------|------|
| `env_id` | 环境唯一标识 | 创建环境时生成 |
| `cred_ver` | 凭据版本号 | 初始为 `1`，密钥吊销后递增 |
| `secret` | 主密钥 | **仅此一次返回**，永不在线传输 |
| `uid` | 用户 ID | AIC 平台账户 |

> **警告**：`secret` 只在创建时展示一次，请妥善保存。丢失后需吊销并重新生成，旧密钥立即失效。

---

## 二、密钥派生

从 `secret` 和 `env_id` 通过 **HKDF-SHA256** 派生三个用途隔离的密钥：

```
K_connect  = HKDF-SHA256(secret, salt=env_id, info="aic/env/connect/v1")    → 32 字节，Base64URL
K_server   = HKDF-SHA256(secret, salt=env_id, info="aic/env/server-proof/v1") → 32 字节，Base64URL
K_tool     = HKDF-SHA256(secret, salt=env_id, info="aic/env/tool-request/v1") → 32 字节，Base64URL
```

| 密钥 | 用途 |
|------|------|
| `K_connect` | 签注连接 token，用于 NATS 鉴权 |
| `K_server` | 服务端 challenge 验证（预留） |
| `K_tool` | 验签工具请求，防止伪造指令 |

---

## 三、连接与鉴权

### 3.1 NATS 连接参数

| 参数 | 值 | 说明 |
|------|-----|------|
| URL | `wss://ivec.ai/aic/api/nc` | NATS over WebSocket |
| 认证方式 | Token | 动态生成连接 token |
| Name | `aic-env-<env_id>` | **建议设置**，便于服务端日志追踪 |

每次连接（含自动重连）需新生成一个 token，不能复用。

### 3.2 连接 Token 格式

```
e1.<env_id>.<env_info_b64>.<ts_ms>.<nonce_b64>.<sig_b64>
```

| 字段 | 说明 | 约束 |
|------|------|------|
| `e1` | 协议版本前缀 | 固定值 |
| `env_id` | 环境 ID | 不含 `.` `*` `>` 和空白字符 |
| `env_info_b64` | 设备信息 JSON 的 Base64URL 编码 | JSON 原文 ≤ 1024 字节 |
| `ts_ms` | Unix 毫秒时间戳 | 与服务器时钟偏差需在 ±60s 内 |
| `nonce_b64` | 随机数 Base64URL | 16 字节，每次连接必须不同 |
| `sig_b64` | HMAC-SHA256 签名 Base64URL | 32 字节 |

### 3.3 env_info JSON 结构

```json
{
  "env_version": "1.0.0",
  "env_type": "device",
  "env_name": "my-raspberry-pi"
}
```

| 字段 | 可选值 | 说明 |
|------|--------|------|
| `env_version` | 任意字符串 | 客户端版本号 |
| `env_type` | `sandbox` / `device` / `server` | 设备类型 |
| `env_name` | 任意字符串 | 设备名称（展示用） |

### 3.4 签名计算

签名输入为**紧凑 JSON**（字段顺序固定，无空格无换行）：

```json
{"domain":"aic-env-connect-v1","env_id":"<env_id>","uid":"<uid>","env_info":"<env_info JSON 原文，非 Base64>","unix_ms":<整数>,"nonce":"<nonce_b64>"}
```

```
sig = HMAC-SHA256(K_connect, 签名输入)
sig_b64 = Base64URL(sig)
```

> 签名输入必须是紧凑格式（无空格、无换行），字段键名固定不可更改。env_info 使用 JSON 原文而非 Base64 编码值。

### 3.5 连接被拒

若同一凭据已有活跃连接，新连接会被拒绝。客户端应**退避重试**（建议 1s → 2s → 4s → 8s），等待旧连接释放后自动重连。

---

## 四、发布能力声明 (CAPS)

NATS 连接成功后，**立即发布**能力声明到：

```
u.<uid>.e.<env_id>.<cred_ver>.caps
```

每次重连成功后也必须重新发布。

### 载荷格式

```json
{
  "env_id": "<env_id>",
  "agent_version": "1.0.0",
  "credential_ver": 1,
  "device_type": "device",
  "device_name": "my-raspberry-pi",
  "device_info": {
    "hostname": "raspberrypi",
    "os": "linux",
    "arch": "arm64",
    "num_cpu": 4,
    "go_version": "go1.24"
  },
  "tools": [
    {
      "name": "exec",
      "description": "Execute a program. action is the program name (bash, sh, ls, python, git, ...), argv are its arguments.",
      "parameters": {
        "type": "object",
        "properties": {
          "action": {"type": "string", "description": "Program name or shell to execute"},
          "argv": {"type": "array", "items": {"type": "string"}}
        },
        "required": ["action", "argv"]
      },
      "required_level": 2,
      "policy_version": "1"
    },
    {
      "name": "fs",
      "description": "File system operations: ls, read, write, edit, rm, mkdir, cp, mv, search.",
      "parameters": {
        "type": "object",
        "properties": {
          "action": {"type": "string", "enum": ["ls","read","write","edit","rm","mkdir","cp","mv","search"]},
          "argv": {"type": "array", "items": {"type": "string"}}
        },
        "required": ["action", "argv"]
      },
      "required_level": 1,
      "policy_version": "1"
    }
  ]
}
```

### tool 字段说明

| 字段 | 类型 | 说明 |
|------|------|------|
| `name` | string | 工具名（仅含字母数字和下划线），对应订阅主题 `tool.<name>.req` |
| `description` | string | 工具描述，供 LLM 理解工具用途 |
| `parameters` | object (JSON Schema) | 工具输入参数的 JSON Schema 定义 |
| `required_level` | int | 权限等级：1=Confirm, 2=Auto, 3=Allow。低于此等级的调用会被拒绝 |
| `policy_version` | string | 策略版本标识（可自定义） |

> 至少需声明 `exec` 和 `fs` 两个内置工具，否则 LLM 无法执行命令和操作文件。自定义工具使用 `env_run` 调用。

---

## 五、心跳 (Presence)

每 **20 秒**发布一次心跳到：

```
u.<uid>.e.<env_id>.<cred_ver>.presence
```

### 载荷格式

```json
{
  "env_id": "<env_id>",
  "credential_ver": 1,
  "running": 1,
  "sent_at": "2026-07-17T10:30:00Z"
}
```

| 字段 | 说明 |
|------|------|
| `running` | 固定为 `1`（表示在线） |
| `sent_at` | RFC3339 UTC 时间戳 |

> 心跳中断后，服务端会在两次心跳周期内标记环境为 offline。建议客户端在后台 goroutine/线程中持续发送，网络抖动时适当容忍。

---

## 六、订阅工具请求

订阅通配符主题接收工具调用：

```
u.<uid>.e.<env_id>.<cred_ver>.tool.*.req
```

当 LLM 调用 `env_exec`、`env_fs` 或 `env_run` 时，服务端会向对应主题发送请求：

| 工具 | 主题 |
|------|------|
| `exec` | `u.<uid>.e.<env_id>.<cred_ver>.tool.exec.req` |
| `fs` | `u.<uid>.e.<env_id>.<cred_ver>.tool.fs.req` |
| 自定义工具 | `u.<uid>.e.<env_id>.<cred_ver>.tool.<tool_name>.req` |

### 6.1 请求载荷

```json
{
  "msg_id": "msg_abc123",
  "session_id": "session_xyz",
  "tool_name": "exec",
  "tool_data": {
    "action": "ls",
    "argv": ["-la", "/tmp"]
  },
  "granted_level": 3,
  "nonce": "abc123def456",
  "deadline": "2026-07-17T10:31:00Z",
  "sig": "hmac_base64url_signature",
  "env_id": "abc123",
  "approval": {
    "fingerprint": "approve-abc",
    "resolved_by": "user_id",
    "resolved_at": "2026-07-17T10:30:30Z"
  }
}
```

| 字段 | 说明 |
|------|------|
| `msg_id` | 请求唯一 ID，响应时原样返回 |
| `session_id` | LLM 会话 ID |
| `tool_name` | 工具名 |
| `tool_data` | 工具参数，action+argv 格式（自定义工具自行定义） |
| `granted_level` | LLM 授予的权限等级 |
| `approval` | 审批凭证。存在表示用户已事前批准 |
| `sig` | HMAC-SHA256(K_tool, 签名输入)，请求签名 |
| `deadline` | RFC3339 截止时间，过期请求必须拒绝 |

### 6.2 验签

签名输入为紧凑 JSON（字段顺序固定）：

```json
```

计算过程：

```
toolDataJSON = compact_json(request.tool_data)    // 紧凑 JSON，字段按字母序
toolDataHash = SHA256(toolDataJSON).hex()

                         session_id, tool_name, tool_data_sha256: toolDataHash,
                         granted_level, nonce, deadline, approval_fingerprint: null})

expected = HMAC-SHA256(K_tool, sigInput)  // 原始字节
actual   = Base64URLDecode(request.sig)

验证通过 ⇔ expected == actual (恒等比较)
```

> 签名无效 → 返回 `{"status": "rejected", "error": "invalid request signature"}`
> deadline 过期 → 返回 `{"status": "rejected", "error": "request deadline exceeded"}`

---

## 七、工具实现

### 7.1 exec — 命令执行

`action` 即要执行的程序名，`argv` 为其参数。`exec.Command(action, argv...)` 直调，不嵌套 shell。

**tool_data**：

```json
// Shell 模式：通过 shell 执行脚本
{"action": "bash", "argv": ["-c", "echo hello && ls /tmp"]}
{"action": "sh", "argv": ["-c", "uname -a"]}
{"action": "powershell", "argv": ["-Command", "Get-Process"]}

// 直接执行命令
{"action": "ls", "argv": ["-la", "/tmp"]}
{"action": "echo", "argv": ["hello world"]}
{"action": "git", "argv": ["status", "--short"]}
```

**响应示例：**

```json
// 成功
{
  "status": "completed",
  "content": "hello world\nfile1.txt\n",
  "attrs": {
    "path": "/tmp/aic/sess_xyz/msg_abc.log",
    "rows": "2",
    "truncated": "false"
  }
}

// 命令执行失败 (exit code != 0)
{
  "status": "error",
  "error": "exec: exit status 1 (exit=1)",
  "content": "ls: /nonexist: No such file or directory\n",
  "attrs": {
    "path": "/tmp/aic/sess_xyz/msg_abc.log",
    "rows": "1",
    "truncated": "false"
  }
}

// 超时 (50s，仍保留已输出内容)
{
  "status": "error",
  "error": "exec: command timed out after 50s",
  "content": "(超时前的部分输出，前 1000 行)",
  "attrs": {
    "path": "/tmp/aic/sess_xyz/msg_abc.log",
    "rows": "1000",
    "truncated": "true"
  }
}

// 签名无效或权限不足
{
  "status": "rejected",
  "error": "insufficient permission: exec requires level 2, got 1"
}
```

**输出说明：**

- 命令 stdout 和 stderr 合并写入 `/tmp/aic/{session_id}/{msg_id}.log`
- 完整日志通过日志文件存放，Agent 可用 `env_fs → read` 查看
- 响应中 `content` 最多返回前 **1000 行**，`attrs.truncated` 标识是否被截断
- 日志路径为系统临时目录下 `aic/{session_id}/{msg_id}.log`（Linux: `/tmp/...`，macOS: `$TMPDIR/...`，Windows: `%TEMP%\...`
- `attrs.path` 指向完整日志文件路径

| attrs | 说明 |
|-------|------|
| `path` | 日志文件路径 |
| `rows` | 返回的行数 |
| `truncated` | `"true"` / `"false"`，输出是否被截断 |

**实现要点**：
- `exec.Command(action, argv...)` 直调，action 可以是 shell 也可以是任意可执行文件
- 子进程设置独立进程组（`setpgid`），防止 kill 影响 agent 自身
- 超时 50 秒（低于 NATS 请求超时 60 秒）

### 7.2 fs — 文件操作

参数格式：`{ action, argv: [...] }`，argv 中位置参数和 `--flag` 可混合排列。

#### ls — 列出目录

```
输入: { "action": "ls", "argv": ["/tmp"] }
```

```
响应:
{
  "msg_id": "req_xxx",
  "status": "completed",
  "content": "dir1/\ndir2/\nfile1.txt\nfile2.txt\n",
  "attrs": {
    "action": "ls",
    "path": "/tmp",
    "rows": "4",
    "path_kind": "mixed",
    "truncated": "false"
  }
}
```

| attrs | 说明 |
|-------|------|
| `path` | 目录路径 |
| `rows` | 条目数 |
| `path_kind` | `file` / `dir` / `mixed` |
| `truncated` | 输出是否截断 |

#### read — 读取文件

```
输入:  { "action": "read", "argv": ["/etc/hosts", "--offset", "0", "--limit", "100"] }
输入:  { "action": "read", "argv": ["/tmp/photo.png"] }
```

```
文本响应:
{
  "status": "completed",
  "content": "127.0.0.1 localhost\n::1 localhost\n",
  "attrs": {
    "action": "read",
    "path": "/etc/hosts",
    "mime": "text/plain",
    "rows": "10",
    "range": "1-10",
    "truncated": "false"
  }
}

图片响应:
{
  "status": "completed",
  "content": "Image file: /tmp/photo.png (image/png, 245760 bytes)",
  "attrs": {
    "action": "read",
    "path": "/tmp/photo.png",
    "mime": "image/png",
    "size": "245760",
    "image_path": "/tmp/photo.png"
  }
}
```

| attrs | 说明 |
|-------|------|
| `path` | 文件路径 |
| `mime` | 如 `text/plain`、`image/png` |
| `rows` | 文本文件总行数 |
| `range` | `"start-end"` 返回的行范围 |
| `truncated` | 是否因 offset/limit 截断 |
| `size` | 二进制文件字节数 |
| `image_path` | 图片文件路径，供 UI 渲染 |

#### write — 写入文件

```
输入: { "action": "write", "argv": ["/tmp/out.txt", "--content", "hello world"] }
```

```
响应:
{
  "status": "completed",
  "content": "wrote file: /tmp/out.txt",
  "attrs": { "action": "write", "path": "/tmp/out.txt" }
}
```

#### edit — 编辑文件

```
输入:  { "action": "edit", "argv": ["/tmp/config.ini", "--old", "port=8080", "--new", "port=9090"] }
输入:  { "action": "edit", "argv": ["/tmp/config.ini", "--old", "TODO", "--new", "DONE", "--replace-all"] }
```

```
响应:
{
  "status": "completed",
  "content": "replaced 1 occurrence(s) in /tmp/config.ini",
  "attrs": { "action": "edit", "path": "/tmp/config.ini" }
}
```

| flag | 说明 |
|------|------|
| `--old` | 精确匹配的原文（含空格），必填 |
| `--new` | 替换后的文本 |
| `--replace-all` | 替换所有出现，默认仅首次 |

#### rm — 删除

```
输入: { "action": "rm", "argv": ["/tmp/old_file.txt"] }
```

```
响应:
{
  "status": "completed",
  "content": "removed /tmp/old_file.txt",
  "attrs": { "action": "rm", "path": "/tmp/old_file.txt" }
}
```

#### mkdir — 创建目录

```
输入: { "action": "mkdir", "argv": ["/tmp/new_dir"] }
```

```
响应:
{
  "status": "completed",
  "content": "created /tmp/new_dir",
  "attrs": { "action": "mkdir", "path": "/tmp/new_dir" }
}
```

#### cp — 复制

```
输入: { "action": "cp", "argv": ["/tmp/src.txt", "/tmp/dst.txt"] }
```

```
响应:
{
  "status": "completed",
  "content": "copied /tmp/src.txt to /tmp/dst.txt",
  "attrs": { "action": "cp", "path": "/tmp/dst.txt" }
}
```

#### mv — 移动/重命名

```
输入: { "action": "mv", "argv": ["/tmp/old.txt", "/tmp/new.txt"] }
```

```
响应:
{
  "status": "completed",
  "content": "moved /tmp/old.txt to /tmp/new.txt",
  "attrs": { "action": "mv", "path": "/tmp/new.txt" }
}
```

#### search — 搜索文件

```
输入: { "action": "search", "argv": ["/src", "--glob", "*.go", "--pattern", "TODO", "--limit", "20"] }
输入: { "action": "search", "argv": ["/logs", "--pattern", "ERROR", "--ignore-case"] }
```

```
响应:
{
  "status": "completed",
  "content": "1\t/src/main.go\t// TODO: optimize\n2\t/src/util.go\tTODO handle error\n",
  "attrs": {
    "action": "search",
    "path": "/src",
    "rows": "2",
    "truncated": "false"
  }
}
```

| flag | 说明 |
|------|------|
| `--glob` | 文件名匹配（`*` `?`），不指定则匹配所有文件 |
| `--pattern` | 文件内容子串匹配 |
| `--limit` | 默认 100，最大 500 |
| `--ignore-case` | bool flag，忽略大小写 |

| attrs | 说明 |
|-------|------|
| `path` | 搜索根目录 |
| `rows` | 匹配条目数 |
| `truncated` | 是否因 limit 截断 |

#### argv 解析规则

- 非 `--` 开头的为位置参数
- `--key value` 为键值对（下一个非 `--` 开头的为 value）
- `--key` 单独出现为 bool 标记
- 提取 flag 时必须创建新切片，避免引用同一底层数组导致后续 flag 被覆盖

---

## 八、幂等缓存

按 `msg_id` 缓存 `status=completed` 的响应。相同 `msg_id` 重试时直接返回缓存，`waiting` / `rejected` / `error` 不缓存。

---

## 九、完整生命周期

```
1. 获取凭据
   env create → env_id + secret
   凭据格式: <env_id>.<cred_ver>.<secret>.<uid>

2. 密钥派生
   secret + env_id → HKDF-SHA256 → K_connect, K_tool

3. 连接 NATS
   WebSocket: wss://ivec.ai/aic/api/nc
   认证: Token 认证（每次连接动态生成 e1.xxx token）
   连接名: aic-env-<env_id>

4. 发布 CAPS
   → u.<uid>.e.<env_id>.<cred_ver>.caps (声明能力和设备信息)

5. 订阅工具请求
   ← u.<uid>.e.<env_id>.<cred_ver>.tool.*.req (通配符)

6. 启动心跳
   → u.<uid>.e.<env_id>.<cred_ver>.presence (每 20s)

7. 处理工具请求
   ← 收到 → 验签 → 执行 → 响应

8. 重连
   断开后自动重连 → 回到步骤 3
   重新生成 token → 重新发布 CAPS
   若被拒（已有连接）→ 退避重试

9. 断开
   停止心跳 → 服务端标记 offline
```

---

## 十、安全约束

- `secret` 永不在线传输，仅用于本地密钥派生和签名
- 所有工具请求签名为 HMAC-SHA256(K_tool, canonical JSON)，防伪造指令
- 过期 deadline 的请求必须拒绝
- 签名无效的请求必须拒绝
- `env_id` / `uid` / `cred_ver` 中不含 `.`、`*`、`>` 和空白字符
- `env_info` JSON 原文 ≤ 1024 字节
- 设备端 NATS 权限自动限域：只能访问 `u.<uid>.e.<env_id>.<cred_ver>.` 范围内的主题
- 同一凭据同时只能有一个活跃连接，重复连接会被拒绝

---

## 附录 A：HKDF 参考（伪代码）

```
function deriveKey(secret, envID, info):
    salt = envID
    return HKDF-SHA256(
        secret = secret,
        salt   = salt,
        info   = info,
        length = 32          // 输出 32 字节
    )
    // 返回值：Base64URL 编码的 32 字节密钥

K_connect = deriveKey(secret, envID, "aic/env/connect/v1")
K_tool    = deriveKey(secret, envID, "aic/env/tool-request/v1")
```

## 附录 B：连接 Token 生成（伪代码）

```
function generateToken(envID, uid, envVersion, envType, envName, kConnect):
    envInfo   = JSON.stringify({env_version: envVersion, env_type: envType, env_name: envName})
    // envInfo 原文 ≤ 1024 字节
    envInfoB64 = Base64URL(envInfo)
    ts        = currentUnixMillis()
    nonce     = randomBytes(16)
    nonceB64  = Base64URL(nonce)

    sigInput  = JSON.stringify({
        domain:   "aic-env-connect-v1",
        env_id:   envID,
        uid:      uid,
        env_info: envInfo,      // 注意：JSON 原文，非 Base64
        unix_ms:  ts,
        nonce:    nonceB64
    })  // 紧凑格式，无空格无换行

    sig    = HMAC-SHA256(kConnect, sigInput)
    sigB64 = Base64URL(sig)

    return "e1." + envID + "." + envInfoB64 + "." + ts + "." + nonceB64 + "." + sigB64
```

## 附录 C：工具请求验签（伪代码）

```
function verifyRequest(request, kTool):
    toolDataJSON   = JSON.stringify(request.tool_data)  // 紧凑，字段按字母序
    toolDataHash   = SHA256(toolDataJSON).hex()

    sigInput = JSON.stringify({
        version:              1,
        env_id:               request.env_id,
        msg_id:               request.msg_id,
        session_id:           request.session_id,
        tool_name:            request.tool_name,
        tool_data_sha256:     toolDataHash,
        granted_level:        request.granted_level,
        nonce:                request.nonce,
        deadline:             request.deadline,
        approval_fingerprint: null
    })  // 紧凑格式，无空格无换行

    expectedSig = HMAC-SHA256(kTool, sigInput)
    return constantTimeEquals(expectedSig, Base64URLDecode(request.sig))
```

## 附录 D：argv 解析（伪代码）

```
function parseArgv(argv):
    positional = []
    flags      = {}
    bools      = {}

    for i in 0..len(argv):
        if argv[i] starts with "--":
            key = argv[i][2:]
            if i+1 < len(argv) and not argv[i+1] starts with "--":
                flags[key] = argv[i+1]
                i = i + 1
            else:
                bools[key] = true
        else:
            positional.append(argv[i])

    return {positional, flags, bools}
```

> 提取 flag 时注意创建新数组再 append，避免 slice aliasing 导致后续参数被覆盖。
