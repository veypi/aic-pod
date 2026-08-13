# Host 沙箱设计方案

> 状态：设计已确认，待实施
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
- **审批即等级**：沙箱决策输出直接映射为权限等级；执行端只做 `granted ≥ required` 数字比较，审批通过 = procs 以 granted = 9 重发（无 fingerprint），不发明新协议（详见 aic `docs/tool_permission.md`）。

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
- **exec `nosandbox` 参数**（仅物理 host）：AI 可显式请求免沙箱执行——required 提升 Critical(4) 必转人工审批（两端各自独立判定：server procs CheckLevel + host checkGranted 纵深）；审批通过后 granted 9 + nosandbox 标记随请求下发，exec_procs 仅据标记免沙箱。sudo 等提权需求统一走此通道（不引入独立命令）。
- **后端**：linux = bubblewrap（功能性 probe 后缓存）；darwin = sandbox-exec（Seatbelt，固定 /usr/bin 路径）；windows = 受限令牌（CreateRestrictedToken，WRITE_RESTRICTED|LUA|禁用最大特权）+ 能力 SID ACL 写授权（幂等：已有该 SID 完全访问 ACE 则跳过，目录被删重建后自动补授，无进程级缓存状态）；其他平台 fail-closed。
- **workspace-write 可写根**：工作区 + 平台临时区 + 常见工具链缓存目录（`cacheRoots()`，存在性过滤）：darwin `~/Library/Caches`+`~/.npm`；linux `$XDG_CACHE_HOME`（缺省 `~/.cache`）+`~/.npm`；windows 精确子目录 `%LOCALAPPDATA%\{go-build,npm-cache,pip\Cache}`（**不放行整个 LOCALAPPDATA**——其下含大量应用数据）；`$GOCACHE`/`$XDG_CACHE_HOME` 显式设置时并入。缓存投毒风险属可接受边界（沙箱防灾难性破坏，不防持续控制构建链的定向攻击）。
- **.git 保护**：工作区 `.git` 只读覆盖（bwrap ro-bind / seatbelt deny 优先）——保护对象是 bash/rm 等通用命令；**git 自身豁免**（argv[0] basename 匹配，shell 包装不豁免），git 写操作等级由 vcore 子命令分级表承担（add/commit/checkout/switch=2，push/reset=3，checkout pathspec 形态提升 3：`--` 分隔，或无 `--` 但参数呈明显非法 refname 形态——`.`/`..`/`./`/`../` 前缀、绝对路径、结尾 `/`、含 `\` `:` `*` `?` `[` 空格，git refname 不可能含这些，出现即必为路径；残余缺口：`checkout <纯文件名>` 与分支名静态不可区分不提升）。linux 有效性实测（bwrap / Debian 13）：沙箱内嵌套 `unshare -rm` 后 umount ro-bind 覆盖与 re-bind 工作区两种逃逸均被内核挂载归属规则拒绝（挂载属于父 userns）——ro-bind 是有效边界非纸面加固。已知缺口：windows 端无 .git 覆盖；linux 仅保护目录形态 .git（worktree/submodule 的 .git 文件不覆盖）；嵌套子仓库不覆盖。
- **browser 免沙箱**：pod 模式语义即不隔离（§5.6），且沙箱下 Chrome 冷启动必挂（实测）；两端 browser 均 `NoSandbox: true`（host = 用户本机环境，cloud = 服务端会话空间收容）。闸门在服务端审批（browser 声明 level 2）+ host checkGranted 纵深；文件效应（截图/下载/上传）全部由 host 进程经 VFS 完成，不经 CLI 进程。
- **状态目录**：host 工具状态落 `UserConfigDir/aic/`（browser 交换目录 `browser/{sid}`），不落用户工作区（可能是 git 仓库）；配置目录不可用时回落进程级唯一临时目录（`MkdirTemp` 0700 路径不可预测，不用共享 /tmp 固定路径防同名预创建占位）。host browser 截图默认落 `{tmp}/aic/screenshot/`（显式 `-o` 仍写指定路径）。
