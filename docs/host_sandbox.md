# Host 沙箱设计方案

> 状态：设计已确认；主体已实施——实施记录见 §11–13（§10 为原始实施计划）
> 范围：aic-pod 全部客户端形态（desktop 原生 Win/macOS/Linux、容器、未来 embedded）；browser 端无 fs/exec 能力，仅共享决策引擎设计
> 关联文档：[design.md](./design.md)；协议见 aic 仓库 docs/instruction_sets_v2.md §6

---

## 1. 背景与威胁模型

### 1.1 已验证的安全事件（对比测试报告）

| 事件 | 影响 |
|------|------|
| fs read `/etc/shadow`、`/proc/1/environ` 成功 | 任意文件无审批可读 |
| `/proc/1/environ` 泄露 `ENV_KEY` 全串 | 凭证可被 agent 读回，持有者可冒充 host 接入平台 |
| fs write `/etc/hostname`、`/root/*` 成功（容器 root） | 系统关键目录可写 |
| fs search `/proc` 阻塞导致后续所有指令超时 | 单请求阻塞演化为全工具不可用（已修复：并发分发 + 伪文件防护） |

### 1.2 威胁模型

- **不信任的输入**：LLM 生成的工具调用（可能被 prompt injection 操控）。
- **信任边界**：host 拥有者（部署者 + 审批人）。server 转发链路已有签名/防重放保护，不在本方案范围。
- **保护目标优先级**：
  1. host 凭证（ENV_KEY 及其派生密钥）不被读回；
  2. 高危操作（写、提权、持久化、外发）必经人工审批；
  3. 普通能力不被误伤（desktop 全盘设计不变）。

### 1.3 设计原则

- **fs 与 exec 沙箱分离**：fs 防"路径越权"，exec 防"命令越权"，攻击面与绕过方式不同，各自独立配置、独立生效。fs 沙箱不能替代 exec 沙箱（exec 可 `cat` 任意文件），反之亦然。
- **归一化**：平台差异只允许存在于数据（路径归一化参数、VFS 根表、命令类别映射表），规则语法、决策引擎、审批协议全平台唯一。
- **审批即等级**：沙箱决策输出直接映射为权限等级；执行端只做 `granted ≥ required` 数字比较，审批通过 = procs 以 granted = 9 重发（无 fingerprint），不发明新协议（详见 aic `docs/permission.md`）。

---

## 2. 权限等级语义

| required_level | 语义 | 行为 |
|---|---|---|
| 0 | 禁用 | 任何 granted 均拒绝 |
| 1 | 读 | 无副作用：只读取、不改任何状态 |
| 2 | 一般写 | 局部、可逆、低爆炸半径的修改 |
| 3 | 危险读\|写 | 破坏性/外发性/特权性/执行性/自主性；用户可授予的上限 |
| **4（隐藏）** | 超高危 | 只作为 required 出现；**系统不允许用户授予 4** ⇒ required = 4 恒大于 granted，必定转入人工审批 |
| **9** | 审批通过标记 | 仅 procs 在审批通过后的单次下发中使用；用户不可配置 |

- 用户可设置 granted ∈ {0, 1, 2, 3}（0 = 该工具对该用户禁用）。server 在等级设置入口钳制上限 3，这是隐藏级别成立的根基。
- 唯一判定式：`granted ≥ required` → 执行；否则 → waiting。引擎内无特例分支。
- 沙箱规则的 effect 不自创概念，直接产出 required_level：

| 规则 effect | 映射 |
|---|---|
| allow | 保持该 action 基础等级（§6.3） |
| require_approval | required_level = 4 |
| deny | required_level = 0 |

---

## 3. 归一化架构

```
                 ┌────────────────────────────────────────────┐
  原始输入   →   │ 适配层（唯一有平台差异之处，纯数据驱动）       │
                 │   路径 → CanonicalPath                     │
                 │   命令 → CanonicalCommand                  │
                 └─────────────────┬──────────────────────────┘
                                   ↓ 统一中间表示 (IR)
                 ┌────────────────────────────────────────────┐
                 │ 决策引擎（平台无关，唯一一份逻辑）             │
                 │   Decide(ruleSet, ir, requestCtx)          │
                 │     → (required_level, rule_id, reason)    │
                 └─────────────────┬──────────────────────────┘
                                   ↓
                 现有等级检查 + waiting 审批流（不改协议语义）
```

决策引擎接口（两指令集共用形状，各自实现）：

```go
// Decision 是一次沙箱判定的输出。
type Decision struct {
    RequiredLevel int    // 0=禁用 1=读 2=一般写 3=危险读写 4=超高危(必审批)
    RuleID        string // 命中规则 ID（空=未命中，取 action 基础等级），进审计
    Reason        string // 展示给审批人
}

type Sandbox interface {
    DecideFs(action string, paths []CanonicalPath, rc *RequestCtx) Decision
    DecideExec(cmd CanonicalCommand, rc *RequestCtx) Decision
}
```

---

## 4. 路径 IR：CanonicalPath

```go
type CanonicalPath struct {
    Root     string   // "/" | "C:" | "//server/share"（UNC 归一为 // 形式）
    Segments []string // 已 Clean、分隔符统一 /、符号链接已解析
    Key      string   // 按卷大小写敏感性折叠后的匹配 key
    InHome   bool     // 是否位于某用户 home 下
    VFS      string   // "" | "proc" | "sys" | "dev"
}
```

### 4.1 归一化管线（五步，差异全部查表）

| 步骤 | 说明 | 平台差异 → 数据 |
|------|------|----------------|
| 1. 展开 | `~`、`%VAR%`、`$VAR` | home 环境变量名列表 |
| 2. 绝对化 + Clean | `filepath.Abs` + `Clean`，分隔符统一 `/` | 无 |
| 3. 根归一 | 盘符大写；UNC `\\s\sh` → `//s/sh` | 非 Windows 为恒等映射 |
| 4. 符号链接收容 | `EvalSymlinks`；不存在的路径退化为解析已存在最长前缀后拼接 | 无（junction 由 OS 层解析） |
| 5. 大小写折叠 → Key | 按卷属性 fold | win/darwin 默认 fold、linux 不 fold，可覆盖 |

### 4.2 平台 Profile（差异即数据）

```go
type PlatformProfile struct {
    CaseFoldDefault bool              // 卷默认大小写行为
    VFSRoots        map[string]string // "proc"→"/proc", "sys"→"/sys", "dev"→"/dev"
    HomeRoots       []string          // ["/home","/root"] | ["/Users"] | ["C:/Users"]
    DefaultRules    []Rule            // 平台内置规则（统一语法）
}
```

linux / darwin / windows 各一份 profile 数据。新增平台 = 加一份数据，决策引擎零改动。

---

## 5. 规则语法

规则不写字面路径，写语义模式。一份规则文档全平台通用，平台不适用的规则**自然不命中**（非编译期裁剪）。

```yaml
rules:
  - id: ssh-private-keys
    match: { home: true, glob: ".ssh/id_*" }   # 原语1：任意用户 home 相对
    effect: require_approval
    actions: [read, search, cp, mv]

  - id: shadow
    match: { path: "/etc/shadow" }             # 原语2：绝对路径（过同一归一化管线）
    effect: require_approval
    actions: [read, search, cp, mv]

  - id: proc-environ
    match: { vfs: proc, glob: "*/environ" }    # 原语3：伪文件系统
    effect: require_approval
    actions: [read, search]

  - id: workspace-only                        # L1 roots（见 §7.1）
    match: { outside_roots: true }
    effect: require_approval
```

- 三个匹配原语：`home`（home 相对 glob）、`path`（绝对路径）、`vfs`（伪文件系统内 glob）；`outside_roots` 为 roots 越界专用标记。
- 匹配按**路径段**进行（非子串），大小写行为与 CanonicalPath.Key 一致。
- `actions` 限定规则生效的 fs action 子集，缺省 = 全部读类 action。

---

## 6. fs 沙箱

### 6.1 三层策略

```
路径 → [L1 allowed roots] → [L2 protected paths] → [L3 特殊文件防护] → 基础等级评估（§6.3） → 执行
```

| 层 | 规则来源 | 命中结果 |
|----|----------|----------|
| L1 allowed roots | 部署者配置 `FS_ROOTS`（默认空 = 全盘，desktop 设计不变） | 越界 → required = **4**（转人工审批；审批人是 host 拥有者，有权打开自己划的边界） |
| L2 protected paths | 内置默认 + `FS_PROTECTED` 追加（见 §6.2） | 命中 → required = **4** |
| L3 特殊文件 | 引擎内置，不可配置 | 非常规文件（设备/管道/socket）不进入 grep/read 候选；阻塞读可被 ctx 中断（已实现） |

L1/L2 只作用于**读类 action**（ls/read/search + cp/mv 的源路径）；写类 action 的基础等级见 §6.3，不重复判定。

### 6.2 内置 protected paths（按平台 profile）

| 平台 | 规则（统一语法） |
|------|------------------|
| 全平台 | `{home, ".ssh/id_*"}`、`{home, ".aws/credentials"}`、`{home, ".netrc"}`、`{home, ".gnupg/**"}`、`{path, "**/.env"}` |
| Linux | `{path, "/etc/shadow"}`、`{path, "/etc/gshadow"}`、`{vfs:proc, "*/environ"}` |
| macOS | `{home, "Library/Keychains/**"}` |
| Windows | `{home, ".ssh/id_*"}`（home 原语自动展开）、`{path, "C:/Windows/System32/config/SAM"}` |
| 自动注入 | pod 自身凭证文件路径（`-key-file`/`ENV_KEY_FILE` 指定时） |

### 6.3 fs action 等级（与 aic §2.4 一致，host 本地逐调用评估）

| action | required_level |
|--------|----------------|
| ls / read / search | 1 |
| write / edit / mkdir / cp / mv / download | 2 |
| rm | 2（文件/空目录）；3（非空目录——host 本地可直接 stat 精确判定，无 server 侧局限） |
| 沙箱规则命中（L1/L2） | 4 |

判定形态与 server 侧一致：handler 按 action/argv 评估 required 后数字比较，非 caps 静态门槛。

### 6.4 决策流程

```
DecideFs(action, paths, rc):
  for p in paths (读类 action 的源路径集):
    cp = Canonicalize(p)
    if rule = match(L1 outside_roots 或 L2 规则, cp):
        return Decision{4, rule.id, rule.reason}
  return Decision{baseLevel(action, argv), "", ""}   // 见 §6.3，参数相关
```

命中 → handler 返回 `waiting`（state + reason + preview 即可，无需 fingerprint）；server 将该 Message 置 waiting，用户批准后 procs 以 `granted_level = 9` 重发同一 msg_id 的请求（nonce/deadline 重新生成），host 数字比较 `9 ≥ 4` 放行执行。

---

## 7. exec 沙箱

### 7.1 命令 IR：CanonicalCommand

```go
type CanonicalCommand struct {
    Exe      string             // argv[0] basename：去 .exe/.bat 后缀、小写
    ViaShell bool               // sh -c / cmd /c / powershell -c 包装
    SubCmds  []CanonicalCommand // shell 串解析出的管道/串联子命令
}
```

### 7.2 三种模式（部署形态选择，互斥）

| 模式 | 语义 | 适用 |
|------|------|------|
| `open` | 任意命令，仅 exec 自身等级约束（required=3） | desktop 默认，用户自有开发机 |
| `guard` | 敏感**类别**命令动态提升 required = 4 | 推荐默认；诚实边界：模式匹配只提高摩擦，防误操作与明显恶意第一跳，不是硬边界 |
| `allowlist` | argv[0] 精确白名单，**不经 shell**（`exec.Command` 直跑，禁 `sh -c`/`cmd /c`/`powershell -enc`），未列出 → required = 0 | 容器/嵌入式（design.md Phase 2 规划合并） |

### 7.3 类别映射表（guard 模式核心，纯数据按平台一份）

```yaml
# linux / darwin            # windows
sudo:    [privilege-escalation]   runas:              [privilege-escalation]
su:      [privilege-escalation]   Start-Process:      [privilege-escalation]
crontab: [persistence]            schtasks:           [persistence]
systemctl: [persistence]          reg:                [persistence]
curl:    [exfil]                  Invoke-WebRequest:  [exfil]
wget:    [exfil]                  certutil:           [exfil]
nc:      [exfil]                  ssh:                [lateral-movement]
ssh:     [lateral-movement]       psexec:             [lateral-movement]
```

规则只对类别生效：

```yaml
- id: priv-esc
  match: { category: privilege-escalation }
  effect: require_approval   # → required = 4
```

**复合判定（exec × fs 规则复用）**：`cat /etc/shadow` 这类"命令 + 敏感路径"，通过将命令参数过一遍 CanonicalPath 管线并匹配 fs L2 规则判定——两个沙箱经 IR 复用规则，不各写一套。

### 7.4 shell 串解析

`guard` 模式下 `sh -c "a | b && c"` 需 mini-parser 拆出 SubCmds 逐条判定（取最高 required）；`allowlist` 模式不经 shell，天然豁免。Windows 侧解析 `cmd /c`、`powershell -Command` 同理。

### 7.5 可选增强：OS 级硬隔离（Phase 2+，独立排期）

真正防绕过只能靠 OS 机制（指令集无关，同时覆盖 fs/exec）：

| 平台 | 机制 |
|------|------|
| Linux | 容器形态已有；原生可选 bubblewrap（mount ns mask 敏感目录） |
| macOS | `sandbox-exec` seatbelt profile |
| Windows | Job Object + restricted token / AppContainer |

---

## 8. 正交层（不属于任一沙箱，客户端级）

### 8.1 输出 redaction

`Client.respond()` 统一出口，对 Content/Error 扫描并替换：

- ENV_KEY 全串（`<host_id>.<cred_ver>.<secret>.<uid>`）
- secret 段（防单独引用）

替换为 `***`。防 `/proc/*/environ`、`.env`、`ps` 等各种姿势的凭证回读，两沙箱之外的兜底。

### 8.2 凭证注入

优先级：`-key-file` / `ENV_KEY_FILE`（0600 文件，配合 docker secret / k8s secret volume）> `-key`（命令行参数 `ps` 可见，文档标注不推荐）> `ENV_KEY`（进程 env，原生平台同用户可读，文档标注不推荐）。

- desktop `.env` 加载改用 `godotenv.Read()`（只解析不注入进程 env）。
- 注意：`/proc/<pid>/environ` 是 exec 时快照，`unsetenv` 无法擦除，故必须源头不入 env。

### 8.3 审计

- 每次沙箱判定（含未命中）随 host 日志输出 `{rule_id, decision, session_id, msg_id}`；
- server 侧已有 ToolCall 全量记录，审批事件（waiting → approve/reject）需确认落库并可按 host 过滤展示。

---

## 9. 协议与兼容性

- **不改 wire 协议语义**：复用 caps actions 的 action 级 `required_level` 声明（静态部分）与 tool response `waiting`（动态部分）；审批通过 = procs 以 granted = 9 重发，无 fingerprint 字段。
- caps 增加 `policy_version` 声明，供 server 展示/审计。
- browser 端无 fs/exec，不受影响；embedded 直接复用本方案（profile + allowlist 模式）。
- 行为变化对 agent 可见：敏感路径读从"直接返回"变为"等待审批"，tool description 需补一句说明。

---

## 10. 实施计划

### Phase A（P0）：凭证保护 + 等级钳制

| 项 | 仓库/文件 | 内容 |
|----|-----------|------|
| A1 | aic-pod `libs/host/redact.go`（新）+ `client.go` | respond() 出口 redaction（ENV_KEY 全串 + secret 段） |
| A2 | aic-pod `desktop/main.js`（Electron 主进程）/ `cli/main.go` | 后端 env 注入（AIC_PORT_FILE/AIC_DEVICE_TYPE）；新增 `-key-file` |
| A3 | aic-pod `Dockerfile` + README | 非 root `USER`；`ENV_KEY_FILE` 文档 |
| A4 | aic server 等级设置 API | granted ≤ 3 钳制校验 |
| A5 | aic server `tools/fs/fs.go` | host target 收到 `$USER/$AGENT/$SESSION` 前缀路径时提前报错（明确提示 host 不支持路径变量） |

### Phase B（P0）：fs 沙箱主体

| 项 | 文件 | 内容 |
|----|------|------|
| B1 | `libs/sandbox/path.go`（新） | CanonicalPath + 五步归一化管线 |
| B2 | `libs/sandbox/rules.go`（新） | Rule/规则匹配引擎 + Decision |
| B3 | `libs/sandbox/profile_{linux,darwin,windows}.go`（新） | 平台 profile 数据 + 默认规则 |
| B4 | `libs/host/fs.go` | handler 执行前接入 DecideFs；命中 → waiting（state + reason） |
| B5 | `libs/host/fs.go` | handler 自检：按 §6.3 评估 required（含 rm 目录非空判定）后数字比较；caps action 等级声明同步 |
| B6 | 验证点 | waiting → grant → procs 以 granted=9 重发（同 msg_id、新 nonce/deadline）→ host 数字比较放行的端到端链路走通 |

### Phase C（P1）：exec 沙箱

| 项 | 文件 | 内容 |
|----|------|------|
| C1 | `libs/sandbox/cmd.go`（新） | CanonicalCommand + shell mini-parser（sh/cmd/powershell） |
| C2 | `libs/sandbox/categories_{linux,windows}.yaml` → go:embed | 类别映射数据 |
| C3 | `libs/host/exec.go` | 模式开关（open/guard/allowlist）+ DecideExec 接入 |
| C4 | 复合判定 | exec 参数过 CanonicalPath 复用 fs L2 规则 |

### Phase D（P2）：一致性测试与收尾

| 项 | 内容 |
|----|------|
| D1 | `CanonicalPath + RuleSet → Decision` 测试向量 corpus（沿用 globMatch 测试向量锁定做法），aic/pod 两端可对拍的部分对拍 |
| D2 | 符号链接逃逸测试（L1 roots 下 `ln -s /etc/shadow` 变体） |
| D3 | tool description 补充审批行为说明；docs 更新 |
| D4 | OS 级硬隔离（bubblewrap / sandbox-exec / Job Object）调研与原型 |

### 验收标准

- 报告攻击链全部失效：`read /etc/shadow`、`read /proc/1/environ`、`search /etc --pattern *password*` 均转审批；审批拒绝后无内容泄露；
- 任何响应中不出现 ENV_KEY 全串或 secret 段；
- granted 无法设置为 4；
- 普通路径读写行为与现状一致（回归测试）。

---

## 11. 实施记录：§5.10 进程沙箱（exec_procs 沙箱层，2026-08-14）

§7.5 的 OS 级硬隔离已落地为 exec_procs 统一沙箱层（`libs/exec_procs/sandbox*.go`）：

- **判定**：未显式 `NoSandbox` 的进程调用一律进沙箱——**审批通过（LevelApproved 9）也不例外**：9 只是「审批通过」的等级语义，免沙箱唯一通道是显式 nosandbox 标记。level 0（未设置/异常）按 read-only 兜底（fail-closed）。无可用后端拒绝执行，绝不静默裸跑。
- **全局开关 `no_sandbox`**（2026-08-19，隐藏配置）：`cfg.Options.NoSandbox`（配置文件 `no_sandbox: true` / flag `-no_sandbox` / env `NO_SANDBOX`）置 true 后**所有** exec 调用跳过沙箱包装（与请求级 nosandbox 同效，无需审批）——等同放弃进程级隔离，仅建议本机可信环境。**隐藏配置**：不经 desktop UI 与本地 API（get_config/set_config）暴露，只能改配置文件/flag/env。注入链：cfg → optionsOf → `Manager.NoSandbox` → Start 判定（`!opts.NoSandbox && !m.NoSandbox`）；运行时修改需重启生效（cli/desktop 启动时读配置）。
- **exec `nosandbox` 参数**（仅物理 host）：AI 可显式请求免沙箱执行——required 提升 Critical(4) 必转人工审批（两端各自独立判定：server procs CheckLevel + host checkGranted 纵深）；审批通过后 granted 9 + nosandbox 标记随请求下发，exec_procs 仅据标记免沙箱。sudo 等提权需求统一走此通道（不引入独立命令）。
- **后端**：linux = bubblewrap（功能性 probe 后缓存）；darwin = sandbox-exec（Seatbelt，固定 /usr/bin 路径）；windows = 受限令牌（CreateRestrictedToken，WRITE_RESTRICTED|LUA|禁用最大特权）+ 能力 SID ACL 写授权（幂等：已有该 SID 完全访问 ACE 则跳过，目录被删重建后自动补授，无进程级缓存状态）；其他平台 fail-closed。
- **workspace-write 可写根**：工作区 + 平台临时区 + 常见工具链缓存目录（`fsauth.CacheRoots()`，存在性过滤；**单一事实源**——fs 权限判定（Decide 白名单）与沙箱 bind 读同一份名单，v0.14.5 §2）：darwin `~/Library/Caches`+`~/.npm`+`~/.cargo`+`~/.rustup`+`~/.m2`+`~/.gradle`+`~/.bun`+`~/.mix`+`~/.cabal`+`~/.local/share/pnpm`+`~/.composer`+`~/Library/Developer/Xcode/DerivedData`；linux `$XDG_CACHE_HOME`（缺省 `~/.cache`）+`~/.npm`+同套工具链目录；windows 精确子目录 `%LOCALAPPDATA%\{go-build,npm-cache,pip\Cache,pnpm,deno,Yarn\Cache,uv\cache}` + `%USERPROFILE%` 下工具链目录（**不放行整个 LOCALAPPDATA**——其下含大量应用数据）；`$GOCACHE`/`$XDG_CACHE_HOME` 显式设置时并入。追加根：`StartOptions.WriteRoots` = cfg `fs_allow` + `grant fs` 临时授权（每次 Start 读当次值，动态生效；2026-09-07 改名，§12）。缓存投毒风险属可接受边界（沙箱防灾难性破坏，不防持续控制构建链的定向攻击）。已知冷启动边界：CacheRoots 存在性过滤——工具链缓存目录尚不存在时不在可写名单内，沙箱内首次运行（如首次 `cargo build` 创建 `~/.cargo`）会写失败；逃生 = 先经 fs 审批创建目录或 nosandbox。
- **env 清洗**（v0.14.5）：沙箱进程启动时剥离敏感环境变量——名字按 `_` 分词后整词命中 KEY/SECRET/TOKEN/PASS/PASSWORD/PASSWD/PASSPHRASE/CRED/CREDENTIALS/AUTH 标记（boundary 匹配：子串会误伤 MONKEY/TURKEY；APIKEY 类无边界连写为已知残余缺口）。背景：`/proc/*/environ` mask 不可行（bwrap 无法 bind 运行时 pid 项）且是假安全感（沙箱进程继承 host 全部 env，`env` 即见）。**nosandbox 不清洗**（语义自洽：免沙箱 = 用户显式信任本次执行）。副作用：沙箱内构建依赖私有源（`NPM_TOKEN` 等）会被断——逃生 = 审批 nosandbox。
- **文件权限模型**（v0.14.5，aic docs/todo.md §2）：`fsauth.Policy` 统一文件权限——同一实例注入 vcore `Env.Policy`（fs 工具按 canonical 路径动态升级 required：deny → 0/0 不可审批绕过（**显式 fs_allow 条目命中 → 回落白名单判定**）；白名单 → 1/2；其余 → 1/3 审批/grant 后放行）与沙箱 write bind 白名单。危险名单初始表按平台分表（`deny_{darwin,linux,windows,other}.go` 各表只含本平台特有路径，+ `deny_common.go` 三平台形态一致的通用凭证条目单源——三平台相关路径不同，分表消除跨平台变量展开串扰风险）+ cfg `fs_deny` 在本平台并集叠加（只加不减）。【2026-09-07 改名：fs_deny_paths→fs_deny、fs_write_roots→fs_allow、grant_apply→grant 三域，§12】条目含 `**/.ssh/**`、`**/*.key`、`~/.aws/**` 等凭证、shell 命令历史（`~/.zsh_history` 等）、容器守护进程 socket（`**/docker.sock`/`**/podman.sock`/`/var/run/docker.sock`——connect = 完全控制守护进程 = 主机逃逸，须配沙箱级 connect 隔离，见下）、系统钥匙串与 cookie 库（mac `~/Library/Keychains/**`、`~/Library/Cookies/**`）、浏览器 profile 密钥库（Chrome 系/Edge/Brave/Vivaldi/Arc/Opera/Firefox，三平台路径分表）、密码管理器数据（1Password/KeePassXC/Bitwarden）、容器/VM 运行时数据区（`~/.orbstack/**`——含 sconssh 直通 VM 的 ssh——/`~/.colima/**`/`~/.lima/**`/`~/.rd/**`/`~/.local/share/containers/**`）、browser state 目录 `$HOME/.aic/.cache/browser/**`（目录口径：含全量 cookie 的 browser.json 与保存流程临时文件一并覆盖）。匹配：glob `**` 跨段、`[` 按字面（不支持字符类，与模式预展开检测集同口径）、win/mac 大小写折叠、canonical 判定防 symlink 绕过；模式预展开缓存（`~`/`$VAR`/`%VAR%` 在 Policy 构建/Reconcile 时一次展开，未定义变量条目整条跳过——分表后只防御用户 cfg 条目；评审修复史：`~` 不展开曾致家目录条目全部死模式，单张跨平台表时代 `%LOCALAPPDATA%` 在 unix 为空曾退化成任意位置匹配）；grant_apply（required 4 必审批）申请白名单——`--temp` 会话内存（host 无 session 结束钩子，grants 不主动清理，量小重启清零）/ `--permanent` 写 `fs_allow`（幂等 = canonical 口径比较；set_config 传空数组可清空名单，是 permanent 的唯一回撤出口）。【2026-09-07 已改名 grant 三域，§12.2】。【2026-09-09：allow 覆盖 deny——fs_allow 显式条目（裸路径=子树 / 通配=精确 glob）压过 fs_deny，内建根与临时 grant 不压，详见 §5.10 deny 隔离】
- **.git 保护**：工作区 `.git` 只读覆盖（bwrap ro-bind / seatbelt deny 优先）——保护对象是 bash/rm 等通用命令；**git 自身豁免**（argv[0] basename 匹配，shell 包装不豁免），git 写操作等级由 vcore 子命令分级表承担（add/commit/checkout/switch=2，push/reset=3，checkout pathspec 形态提升 3：`--` 分隔，或无 `--` 但参数呈明显非法 refname 形态——`.`/`..`/`./`/`../` 前缀、绝对路径、结尾 `/`、含 `\` `:` `*` `?` `[` 空格，git refname 不可能含这些，出现即必为路径；残余缺口：`checkout <纯文件名>` 与分支名静态不可区分不提升）。linux 有效性实测（bwrap / Debian 13）：沙箱内嵌套 `unshare -rm` 后 umount ro-bind 覆盖与 re-bind 工作区两种逃逸均被内核挂载归属规则拒绝（挂载属于父 userns）——ro-bind 是有效边界非纸面加固。已知缺口：windows 端无 .git 覆盖；linux 仅保护目录形态 .git（worktree/submodule 的 .git 文件不覆盖）；嵌套子仓库不覆盖。
- **browser 免沙箱**：pod 模式语义即不隔离（§5.6），且沙箱下 Chrome 冷启动必挂（实测）；两端 browser 均 `NoSandbox: true`（host = 用户本机环境，cloud = 服务端会话空间收容）。闸门在服务端审批（browser 声明 level 2）+ host checkGranted 纵深；文件效应（截图/下载/上传）全部由 host 进程经 VFS 完成，不经 CLI 进程，且四通道均过 `Env.CheckPolicy` 文件权限门（deny 名单/会话 grant 在此生效）。
- **状态目录**（v0.14.5 §4 布局，与 cloud 同构）：会话产物落 `$HOME/.aic/sessions/{sid}/`（exec 日志 `.exec/`、browser 交换 `.browser/`、截图 `.screenshot/`）；browser state（cookies/storage）= `$HOME/.aic/.cache/browser/browser.json`（用户级共享单文件，实例创建自动 load、动作后防抖 1s 按站点 merge 保存——不同会话访问不同站点不互覆；导出临时文件名带实例唯一号，防同用户多实例并发互踩）。均不落用户工作区（可能是 git 仓库）；PublicDir 不可得的罕见场景回落系统临时目录旧位（`{tmp}/aic/{sid}`）。
- **资源限制**（2026-08-28 补齐：此前只有文件隔离，沙箱内命令可无限分配内存/派生进程，实测打爆系统内存死机）：三端同一组上限（`resourceLimit*` 常量，sandbox.go），read-only 与 workspace-write 同限（与文件隔离正交）——AS 4GiB（防大 malloc 吃满物理内存+swap 假死）、job 内存 8GiB（windows 合计）、NOFILE 1024、CPU 600s（与 30m wall 超时双保险）、FSIZE 1GiB（防写爆磁盘）、CORE 0（禁 core dump）。**不设 NPROC**（2026-08-31 事故修正）：unix RLIMIT_NPROC 按 real-UID 全系统计数、超限即 fork EAGAIN，桌面开发机单 UID 常驻进程数百个，设 256 沙箱内一切命令无法 fork 全瘫；防 fork 炸弹改由 30m wall 超时 + 进程组 killEntry + CPU 600s 兜底。实现：
  - linux：bwrap `--rlimit` 追加（exec 前 setrlimit，强限制，零额外进程）；
  - darwin：`/bin/sh -c 'ulimit ...; exec "$@"'` 包装（Seatbelt 不支持资源限制；RLIMIT 跨 exec 继承、子进程只能降低不能提高、ulimit 失败即 fail-closed）。**macOS 内核不支持 RLIMIT_AS/RLIMIT_DATA**（setrlimit 恒 EINVAL，实测）——大内存分配由 exec_procs 进程组 RSS 监控兜底（`rss_darwin.go`：500ms 轮询 `ps -o rss= -g <pgid>` 合计，超 `min(8GiB, 物理内存/2)` 按 killEntry 语义终止；自行 setsid 脱离进程组的进程不在覆盖内，与 --die-with-parent 同边界）；
  - windows：Job Object（`JOBOBJECT_EXTENDED_LIMIT_INFORMATION`：进程内存 4GiB / job 内存 8GiB / 活动进程 256，超限分配失败），spawn 成功后 `AssignProcessToJobObject`（失败 fail-closed：杀进程报错不裸跑），句柄随 cleanup 关闭。
  - exec_procs.Start 挂接：spawn 后（windows）assign job / （darwin）启动 RSS 监控 goroutine，均随 Entry done 退出。
- **deny 隔离**（2026-09-05）：deny 表（fsauth `defaultDenyPaths` + cfg `fs_deny`）接入 exec 沙箱——沙箱默认读全开（seatbelt `allow default` / bwrap 整机 ro-bind），此前 fsauth deny 只约束 fs 工具（文件 API），exec 进程 `cat` 可直读 `.ssh` 等全部凭证（实测报告触发）。链：`Policy.DenyPatterns()`（预展开模式快照，与 fs 判定同一份名单单源）→ `StartOptions.DenyPaths` → 沙箱 profile：
  - darwin seatbelt：每条模式经 `globToSBPLRegex` 转 `(deny file-read* (regex ...))` + `(deny file-write* (regex ...))` + `(deny network-outbound (remote unix (regex ...)))` 三条规则——**读写双拒**（修复前仅拒读，纯写打开仍可改写可写根内 deny 文件，实测 2026-09-05）、**connect 另拒**（AF_UNIX connect() 不走 file-* 判定：docker.sock 文件操作全拒而 `curl --unix-socket` 直通，实测 2026-09-05，docker socket = 主机逃逸；SBPL network 过滤器 `(remote unix (regex ...))` 实测可用，内核对判定路径先规范化——`/tmp`→`/private/tmp` symlink 亦命中）。**规则顺序：deny 表在写白名单之后输出**——SBPL 后匹配覆盖先匹配，先输出时可写根内 deny 条目（工作区 `**/.env`/`*.key` 等）写保护会被 allow subpath 覆盖（.git 覆盖幸存仅因其在白名单后输出；实测修复 2026-09-05）。字面条目以双形态入名单（compileDeny 输出 canonical + 字面形：模式自身是符号链接时——`/var/run/docker.sock` → 厂商 socket——两形态都须命中）。SBPL 无 glob filter；regex 为 POSIX ERE、对完整路径字符串匹配（未锚定=子串命中、`^` 锚定有效、字符类可用，实测）。seatbelt 判定前做路径规范化——大小写变体（`~/.SSH`）与 symlink 跳转均被拒（实测）；deny 文件 stat 被拒、lstat 不受影响（ls -la / git status 正常）。转换保 fs 段语义：`**` 零段（吸收紧邻 `/`：段首 `(.*)?` / 段尾 `(/.*)?` / 段中 `/(.*/)?`）、`*`→`[^/]*`、`?`→`[^/]`、字面转义、整串 `^...$` 锚定（.ssh2 类粘连名不会命中 `**/.ssh/**`）；残余偏差仅段内 `**`（a**b 非标准形态）可跨段 → 超集拒绝（安全方向）。
  - linux bwrap：模式实例化为覆盖挂载（bwrap 无路径规则引擎）——字面目录 `--tmpfs` / 字面文件与 unix socket `--ro-bind /dev/null`（socket 覆盖 = AF_UNIX connect 隔离，对齐 seatbelt network-outbound——修复前 socket 被跳过、docker.sock 直通；仅存在时，每次 Start 实时判定→新创建文件下次覆盖；文件覆盖=读写双拒，目录覆盖=读黑洞＋写入落入临时 tmpfs 不落地；**注意：bwrap 以 /dev/null 挂载覆盖 socket 路径的行为待 linux 真机复核**）；尾 `/**` 剥目录覆盖；单 glob（无 `**`）前缀目录 readdir 枚举；** 开头模式 $HOME 根级锚定（最后一个非 ** 段，`.ssh` 类目录 / `id_ed25519*`、`*.key` 文件 glob）；中间 `**`/含 `[` 无法实例化 → 跳过，**exec 通道无兜底**（fsauth 判定层仅约束 fs 工具/VFS，拦不住进程内 cat）——bwrap deny 隔离是近似层，完整 glob 语义仅 seatbelt。
  - windows：不实现（no-op）——受限令牌 restricting list 必须保留 logon/Everyone/用户 SID（进程初始化依赖），文件读权限普遍授予这些组 → 读全开；per-call 隔离不能改全局 DACL（deny ACE 影响宿主机全部进程）。
  - read-only 与 workspace-write 同隔离（拒绝规则与写等级无关）；`.git` 写覆盖、env 清洗、资源限制不变。
  - **设计内副作用**（实测 2026-09-05）：沙箱内一切带认证的远程操作必失败——git ssh push / 私有仓库拉取读不到 `~/.ssh` 密钥 / `~/.netrc` / `~/.git-credentials`（审批通过不豁免，沙箱不认等级）；容器守护进程 CLI（docker/podman 等）沙箱内无法连接（socket connect 被拒，2026-09-05 connect 隔离后）——容器操作走 nosandbox + 审批；deny 名文件（`.env`/`*.pem` 等）的 stat()/读打开失败（cat/cp/对已改文件的 git diff），lstat 类工具不受影响（ls -la / git status / find）。残余缺口：mac `security` CLI 的 keychain mach IPC 未拦（钥匙串文件本体已拒读，提取受 keychain ACL/口令门控）；沙箱内嵌套 `sandbox-exec` 带 network 规则会 `sandbox_apply` EPERM（嵌套限制，无实际影响——被包装命令不需要自定义网络规则）。

  - **allow 覆盖 deny**（2026-09-09，两键语义；触发：默认表 `**/.env` 拒掉工作区项目 .env，而开发读写是常态）：**显式 `fs_allow` 条目压过 `fs_deny`**——裸路径条目覆盖其子树，带通配条目按 glob 精确匹配（如 `/ws/**/.env` 只放 .env）；命中即回落正常分级（读 1 / 写 2）并豁免 deny 的读写双拒。**内建便利根（工作区/临时区/公共区/缓存/会话区）与临时 grant 不压**——它们是写便利不是信任声明（公共区里的 browser cookie 库、缓存/工作区里的 .env/*.pem/*.key 仍受保护）；要开洞就把条目显式写进 `fs_allow`（grant --permanent 亦落在那里）。deny 本身恒为读写双拒且不可审批（写入凭证目录 = 持久化/注入）；`DenyHit` 保持原始表（grant 仍拒 deny 内目标）。canonical 判定（symlink 跳转出 allow 条目不豁免）。沙箱同源：`Policy.DenyOverridePatterns()`（裸路径 → `<root>/**`，通配原样）→ `StartOptions.DenyOverride`——darwin 在 deny 规则之后追加 file-read*（写级再加 file-write*）allow（SBPL 后匹配覆盖先匹配，序在 `.git` 写保护之前）；linux 跳过被覆盖模式完整覆盖的覆盖挂载目标（文件精确命中 / 目录仅 `<target>/**`，更窄覆盖与 unix socket 保持覆盖——connect 隔离不在 allow 覆盖面内）。
  - **net/ssh 具体度优先**（2026-09-09）：host 恒精确匹配（不支持通配主机），具体度只看端口——数字 > `*`；命中 deny 时仅当存在**更具体**的 allow（具体端口）才放行，同精度 deny 胜。保留 `net_deny localhost:6379` 反杀内建 `localhost:*`；新增「宽拒窄放」用法：`net_deny: ["git.corp.com"]` + `net_allow: ["git.corp.com:443"]` → 仅 443 放行（宽拒受限于 host 精确匹配，只能按 host 粒度）。临时 grant 不压 deny。沙箱快照剔除被窄 allow 压过的端口 `*` deny（deny 模式未放行端口由基线全拒兜底，不降低隔离）。

---

## 12. 实施记录：三域授权模型 + 网络管控 + ssh 工具（2026-09-07）

### 12.1 模型

授权配置统一为 **三域 × 三键**（config.yaml，set_config 动态改、内存即时生效、判定读点归一化）：

```yaml
fs_policy: deny     # deny=仅内建可写根+fs_allow | open=除 fs_deny 全可写
fs_deny: []         # 路径 glob 拒绝名单（0 级不可读写、不可审批；被显式 fs_allow 条目覆盖）
fs_allow: []        # 显式 allow 条目（写白名单 + 压 deny）：裸路径=子树，通配=精确 glob（如 **/.env）
net_policy: open    # open（默认）| deny（锁定模式：沙箱出站 localhost-only）
net_deny: []        # host:port 出站拒绝名单（同精度/更宽 allow 均拒；更具体的 allow 条目可覆盖）
net_allow: []       # host:port 出站白名单（内建 localhost:*）
ssh_policy: deny    # deny（默认）=仅 ssh_allow | open=除 ssh_deny 全通
ssh_deny: []        # host[:port] ssh 目标拒绝名单
ssh_allow: []       # host[:port] ssh 目标白名单（bare host = 全端口）
```

统一判定式：**deny 命中 → 拒，除非存在更具体的 allow（fs：显式 fs_allow 条目命中 → 回落白名单判定；net/ssh：具体端口 allow 压端口 `*` deny；同精度 deny 胜）；policy=open → 未命中 deny 一律放；policy=deny → 仅 allow 放行**。条目形态：fs = 路径 glob（canonical 判定，裸路径条目覆盖子树）；net/ssh = `host:port`（host 小写/尾点/IP 经 netip 归一且**精确匹配**——不支持通配主机；port 数字或 `*`；bare host 归一 `host:*`；`user@` 前缀剥除——用户是认证细节不是网络目标）。临时 grant 不压 deny（三域同口径：grant 是 AI 经审批申请的，不构成信任声明）。配置文件键形态（2026-09-09）：cfg.Options 全字段带 yaml tag（snake_case，与本块/本地 API/文档同名），`UnmarshalYAML` 兼容历史小写字段名形态（`fspolicy`/`workdir`/`fsdeny`…，旧文件照常读入，Save 后自愈）——此前文件实际写小写字段名，与本块示例不一致（手写 snake_case 静默失效）。

实现：`libs/fsauth`（fs 域，glob/canonical 语义）+ 新包 `libs/netauth`（host:port 策略引擎，**一个包两个实例** net/ssh——条目形态/匹配/临时 grant 完全相同）；`cfg.AuthSnapshot()` 单源（读点归一化：空/非法 policy 按域默认，fs/ssh=deny、net=open；防 Global 零值把 net 误锁）。重命名（测试版无兼容包袱）：`fs_write_roots → fs_allow`、`fs_deny_paths → fs_deny`——旧键经 flags.LoadCfg 非严格解析静默忽略。

### 12.2 grant 命令（grant_apply 改名三域，cloud 排除）

```
exec grant fs  <path>        [--temp|--permanent]
exec grant net <host:port>   [--temp|--permanent]
exec grant ssh <host[:port]> [--temp|--permanent]
```

单命令 Critical(4) 必审批（vcore 分级表）；显式域前缀不做自动判别（path 与 host:port 有歧义带——Windows `C:\...` 带冒号、相对路径无前导 /）。--temp=域 Policy 会话内存（重启/跨 session 失效）；--permanent=写对应域 allow 列表落盘（`persistGrant` 单一路径：LoadFile → 归一化幂等去重 → Save → `syncAuth` 三域 Reconcile）；域 deny 名单内的目标拒绝申请。回显该域当前完整白名单。

### 12.3 ssh 一级工具（host 专属，cloud 排除；Danger(3)）

`exec ssh <target> [remote command...]`（target = `[user@]host[:port]` 或 `~/.ssh/config` 别名；`host:port` 形态由工具剥端口转 `-p`——ssh CLI 本身不收 host:port）。目标闸 = ssh 域 Policy：pod 进程内 `ssh -G <target>` 拿生效 hostname/port（别名解析复用 ssh 原生逻辑）→ `Allowed(sid, host, port)`，未命中报错并提示 `grant ssh host:port`。**免沙箱内置执行**（ssh 需读 `~/.ssh` 密钥/config——fs_deny 0/0 不可审批绕过，沙箱化必须破例开洞反而破坏 deny 语义；目标审批即授权密钥使用；与 browser 同属 host 内部管控调用方）。工具固定 flag 拼装：目标前不接受任何用户 flag（`-o`/`-L`/`-R`/`-D`/`-W`/`ProxyCommand` 等可注命令开端口的形态从根上不存在），目标后参数原样作为远端命令（ssh 自身语义：目标后不再解析 flag）；固定 `ConnectTimeout=10` + `StrictHostKeyChecking=accept-new`（目标审批即信任决策）。认证 = 设备已有密钥/ssh-agent/config；**密码形态 v1 不可用**（exec 无 TTY，密码进 argv 会泄漏进日志——待平台审批框支持密码字段）。

**scp 一级工具**（2026-09-07，同通道同权限）：`exec scp [-r] [-p] [-q] [-P port] <source> <target>`——恰好一端为远端（`[user@]host:path`，首个冒号先于首个斜杠 = 远端；Windows 盘符 `C:\`/`C:/` 判本地）；远端↔远端不支持（经本机两步），双本地报错引导 cp。远端目标闸 = ssh 域 Policy（与 ssh 完全同一套，含 `ssh -G` 别名/端口解析与 grant 引导）；**本地侧 fs 门控**：本地操作数过 fsauth 判定（deny 0/0 拒绝；写等级不另查——base Danger(3) 能执行即 granted>=3 覆盖 2/3），且本地操作数解析为绝对路径后回写 argv（不依赖 pod 进程 cwd）——scp 免沙箱执行，这层工具侧判定是对本地文件系统的必要补偿。flag 白名单仅 `-r`/`-p`/`-q`/`-P`（scp 远端语法 host:path 无法携带端口，`-P` 是唯一端口通道且结果同样过闸）；`-F`/`-o`/`-S`/`-J`/`-i`/`-c`/`-l` 等可注命令/换密钥/绕 config 的形态从根上不存在。固定 `-B`（批模式禁密码提示）+ `ConnectTimeout=10` + `accept-new`。

### 12.4 网络管控（net 域）

**默认 open（2026-09-07 用户定）**：deny 默认会让沙箱内 apt/wget/git clone/npm/pip 等工具链全断（任意二进制联网内核无法按域名放行），AI 开发场景不可用——deny 作锁定模式选用。

**实测结论（2026-09-07，darwin sandbox-exec 探针，逐形态二分）**：seatbelt 网络过滤器 `(remote tcp/ip "host:port")` 的 host **只支持 `*`/`localhost`**（`localhost` 字面覆盖 127.0.0.1/::1，实测）——按目标 IP/域名的内核级放行在现代 macOS 不存在；linux bwrap 本来就只有 `--unshare-net` 全有/全无。两平台内核粒度一致 = localhost-only 或全网。**三条毒/坑形态**：①`(allow network-outbound (local tcp "localhost:*"))` 语义覆盖一切出站连接（本地端恒命中）= 拆掉整个 deny outbound，严禁输出（实测确认）；②`(remote unix ...)` 必须 regex 形态（裸字符串参数非法）；③deny network* 下系统 DNS 不可修复（mDNSResponder unix socket/mach-lookup dnssd.service/udp 53 逐轮实测均无效）——锁定模式 = 沙箱内无 DNS。

由此 deny 锁定模式的落地语义：
- **内核层**：darwin = `(deny network-inbound/outbound)` + net_allow 条目实例化（loopback → `localhost` 两形态：remote tcp 连通 + local tcp inbound bind/listen；非 loopback → **退化为 `*:port` 按端口粗放行**；`port=*` 不可表达跳过）+ net_deny 落地仅 loopback 形态（对抗内建 `localhost:*` allow；**非 loopback deny 不输出内核规则**——基线全拒已覆盖，host 粒度内核不可表达，`*:port` 会株连同端口 allow 目标，工具层 curl 闸精判兜底，2026-09-07 评审修复）；**不放行 DNS**（不可修复，见实测结论③）——FQDN 目标由工具层补偿。linux = `--unshare-net` 全断（含 loopback——新 net ns 的 lo 未配置，白名单粒度不生效，近似层）；windows = no-op。
- **工具层（精细闸）**：host 端 vcore curl 已改道 **shellCurlFetcher**（真 curl 二进制经 `exec_procs.Spawn` 统一沙箱；进程内 net/http fetcher 废除——进程内 fetch 不受沙箱管控，锁定模式会形同虚设）：spawn 前置 `netPol.Allowed(sid, host, port)` 闸——open 模式拦 net_deny、deny 模式要求 net_allow 命中，未命中报错提示 `grant net`；**deny 模式 FQDN 目标 pod 侧预解析 + `--resolve host:port:ip` 钉住**（补偿沙箱内无 DNS，兼防 rebinding；实测 curl --resolve + *:port 粗放行连通正常）。curl 沙箱 profile = read-only + deny 表 + net 快照；`-q` 禁读 `~/.curlrc`，`--proto-redir =http,https` 钉住重定向协议族（2026-09-07 评审加固），body 经 stdin `--data-binary @-`。netauth 条目主机不接受通配（`*` 恒惰性，ParseEntry 显式拒绝——评审修复）。
- **Spawn**：exec_procs 新增管道形态启动（与 Start 共享沙箱包装/env 清洗/令牌注入，不经任务托管；Body 读至 EOF 汇合非零退出错误含 stderr 尾部摘要，Abort/Wait 幂等，ctx 取消即杀）。
- **已知缺口**：curl `-L` 重定向由 curl 进程内部跟随——deny 模式下已放行端口的跨主机跳转内核不可拦（如白名单目标 302 到同端口别站）；shell 裸命令在 deny 模式只有 localhost + 白名单端口粗放行（精细 host 判定管不到任意二进制）；net_deny 在 open 模式仅约束 curl 虚拟指令（shell 命令内核无规则）。**后续方向**：pod 内置 localhost CONNECT 过滤代理 + 沙箱子进程注入 proxy env（git/wget/npm/pip 等尊重 proxy env 的工具链在 deny 模式也可逐目标受控），做完再评估 deny 翻默认。

### 12.5 fs_policy=open（顺带语义新增）

fs 域 policy=open = 写除 fs_deny 全放（2 级，不再逐次审批）：fsauth decide 层 trivial；沙箱层 darwin `(allow file-write*)` 打底 + deny 表收尾、bwrap 整机 rw bind（deny 覆盖挂载仍生效）；`.git` 写覆盖不豁免（沙箱安全网保留，git 写走审批/nosandbox）。windows 无实现（ACL 模型按目录授权，no-op）。

### 12.6 测试

netauth 向量（ParseEntry 归一/通配主机拒绝/Allowed 三判定式/内建 loopback+deny 反杀/temp grant 会话隔离/DenyHit 重叠语义/cfg Reconcile/policy 归一）；seatbelt 网络段形态断言（loopback deny 落地、非 loopback deny 不输出、port=* 跳过）；bwrap unshare-net/fs_open bind 形态；Spawn 四用例（嵌套沙箱环境优雅跳过）；grant argv 解析 + runGrant 域路由/deny 拒绝/temp 生效（旧 grant_apply 裸路径形态必须报错）；ssh splitSSHPort/sshResolve（ssh -G 离线向量）；scp parseSCPArgv（flag 白名单/-P 两形态）/splitSCPOperand（盘符与斜杠序分类）；host buildCurlArgv。全量 go test + 三端交叉编译（linux/windows/freebsd）绿。

### 12.7 系统 CA 只读放行（2026-09-11）

**触发**：沙箱内 curl（默认 CA=/etc/ssl/cert.pem）全失败（error 77）——实测：deny 通用表 `**/*.pem` 命中系统公共 CA bundle（`stat`/读被拒），而网络层完全正常（DNS/TCP/http/`-k` https/loopback 均通）。同类影响一切依赖系统 CA 的 CLI（wget、系统 git-https 等）；node/npm 因自带内置 CA 不受影响。

**修复**：新增**只读**放行通道（不并入 DenyOverride——后者随写级追加 `file-write*`，而写系统 CA 目录 = 自定义信任根注入，必须只读）：

- `fsauth.SystemCAReadPatterns()`（平台清单：darwin `/etc/ssl/cert.pem`、`/etc/ssl/certs/**`、Homebrew `openssl@*`/`ca-certificates`（arm64/intel 双前缀）；linux `/etc/ssl/certs/**`、`/etc/pki/**`、`/usr/share/ca-certificates/**`；windows nil）——与 deny 同口径双形态展开（canonicalPattern + 字面，seatbelt 规范化路径两态命中）；
- `confineSpec.readAllow`（Start/Spawn 组装时内置填入，不经 StartOptions——沙箱后端系统豁免）；
- darwin seatbelt：deny/override 之后追加 `(allow file-read* (regex ...))`，**恒不输出 file-write***；
- linux bwrap：并入 `coveredByOverride`（当前 no-op——deny 实例化对 `**` 开头模式 $HOME 锚定，系统路径本不在覆盖内；并入为对齐/未来防护）。

**验证**：seatbelt 形态断言（`TestSeatbeltSystemCAReadAllow`：allow 在 deny 之后、写级也无 write allow、零值无输出）+ `TestSystemCAReadPatterns` 平台清单（含 canonical 形态）；行为实测：沙箱内 `curl https://…`（默认 CA）直 200。

## 13. 实施记录：windows 盘符虚拟根 + 路径归一收口（2026-09-15）

**背景问题**：旧模型 win host 的 `/` = 当前盘根，多盘只能靠 `C:/D:/…` 绝对路径直达；前端树路径把盘符段拼出 `/C:/…`，host 端盘符识别失效（`toOS` 拼出 `\C:\…` 非法路径）——链接点开必失败；且 `rm /` 不命中任何根保护（`toOS("/")` = 当前盘根 RemoveAll）。

**路径模型（重新设计，无兼容包袱）**：

- **规范形 = `C:/…`**（正斜杠 + 盘符冒号，盘符字母大写；裸 `C:` = 盘符根，严格语义）——与 exec/PowerShell 输出同形，AI 在 exec/fs 间复制零翻译。`C:foo` 盘符相对形态不是盘符路径（proto 归为相对路径）；
- **`/` = 虚拟挂载根**：`OSVFS.ReadDir("/")` 返回盘符挂载列表（`C:`、`D:`…，合成 `virtualDir` 条目，size/mtime 零值）、`Stat("/")` 返回合成目录信息；其余操作作用于 `/` 报 `errVirtualRoot`；非盘符绝对路径（`/x`）一律报错——「当前盘根」语义不复存在；
- **归一收口 = 单一纯函数共用**：`proto.NormalizeDrivePath`（纯语法三端一致：`/C:`、`/C:/…`、`C:\…` → 盘符规范形 + 盘符字母大写；绝对路径先 `path.Clean` 折叠多斜杠再判盘符形——`//C:`、`///C:/x` 归一后不存在；`driveRe` 扩为 `^[A-Za-z]:([\\/]|$)` 支持裸盘符）——`ResolvePath` 主入口与 `OSVFS.winToOS` 执行层兜底共用本函数，双端结果一致、不按 GOOS 分叉（§2.1.1）；
- **vcore.Env.VirtualRoot**（host 注入 `runtime.GOOS == "windows"`）：ls 根层盘符条目不递归、不做 .git 探测（仅显示，进入需显式 `ls C:`——tree 默认深度递归进全部盘符必撞节点上限）；rg 拒绝 `path="/"`（内容搜索与文件列举两模式同拒，提示用盘符路径）；
- **根保护**：`filesystemRoots()` win = 盘符根列表 + `"/"`（`rm /`、`mv /` 命中 DeniedError）；多斜杠前缀（`//C:`）曾绕过等值比较（规则层见 `/C:`、执行层见 `C:\`）——归一共用后消除，回归向量锁定；`fsauth.canonical` 裸盘符先补盘根（`C:` → `C:\`）再 EvalSymlinks（裸盘符的盘符当前目录语义不适用于权限判定）；
- **前端收口**（aic/ui/assets/libs）：`hfs.js resolvePath` 树路径 `/C:/…` → 物理 `C:/…`；`fs.js full()` 为树路径唯一拼接点——物理路径无前导斜杠（盘符 items path `C:/…/name`）时补回，所有端共用兜底。

**测试**：proto 归一向量（裸盘符/前导斜杠/大小写/盘符相对形态）；`winToOS` 纯函数向量 + 虚拟根条目（跨平台）；vcore 虚拟根行为（ls 不递归 + rg 拒绝 + 盘符路径正常）；fsauth 裸盘符 canonical；hfs/fs 前端单测（归一 + items 物理形 + full() 收口）。windows 真机行为（盘符探测、ReadDir/Stat 合成节点）由 win host 验证。
