# AIC Pod 架构

状态：2026-10-26 现行架构，统一命令与策略核心迁移已完成；缓存、darwin 代理和实机收尾见 [../development/todo.md](../development/todo.md)。

调用契约见 [设备执行与工具服务](../reference/hosts-tools.md)；权限与沙箱见 [host 沙箱](../reference/host-sandbox.md)；browser/CUA 设备侧接入见 [browser](../guides/browser.md)。

## 1. Pod 是总集成方

Pod 是设备执行端和三个核心职责的唯一集成点：

1. **统一管理配置**：读取、校验、保存设备配置，维护会话授权和永久授权，为每次调用提供明确的策略快照。
2. **统一管理调度**：建立执行上下文，组装命令表，管理会话、工作目录、环境、取消、超时、日志和后台任务。
3. **统一注册并实现虚拟指令**：向 vsh 注册平台命令、服务命令和由 vbox 支撑的原生程序命令。

Pod 引入两个基础库，但不让它们互相承担职责：

| 组件 | 职责 | 接收的策略 |
| --- | --- | --- |
| **aic-pod** | 配置、授权、调度、命令注册和命令实现 | 管理全部配置并分发 |
| **vsh** | 虚拟 shell：脚本解析、命令解析、统一命令准入和命令调用 | `exec_policy` |
| **vbox** | 原生进程沙箱执行与进程托管 | `fs_policy`、`net_policy` |

vsh 不负责 OS 沙箱，vbox 不负责 shell 或命令注册。两者都不管理 Pod 的配置、会话或审批。

## 2. 统一命令模型

### 2.1 vsh 只看见命令实例

Pod 注册到 vsh 的所有命令都使用同一个命令接口和同一个准入流程：

```text
脚本文本
  → vsh 解析并得到一个命令实例
  → vsh 使用 exec_policy 做命令准入
      → 拒绝：命令实现不被调用
      → 允许：调用命令实现
                    → 命令实现落实自身封装的策略
```

无论命令是 Go 内建、Pod 虚拟命令、MCP 服务命令，还是最终启动原生进程的 vbox 支撑命令，命令准入都只使用 `exec_policy`。

不再保留以下例外：

- `ssh/scp/sftp` 只检查 SSH 策略、绕过 exec 准入；
- `grant`、`bg`、`commands` 等注册命令拥有独立的无门通道；
- 原生程序另设一套准入入口；
- 按 basename 对 SSH/Git 等原生程序设置专门限制。

`exec_policy` 通过后，只表示允许调用该命令实现，不代表其内部文件、网络或 SSH 操作自动放行。

### 2.2 原生命令也是注册的虚拟指令

本文所称“原生命令”，是指命令实现最终启动原生进程；它不是 vsh 中的另一种命令类别。

Pod 为这种命令提供的实现封装了：

```text
vbox 支撑命令
  = 原生程序路径与调用参数
  + 本次 fs_policy / net_policy 快照
  + vbox RunProcess
  + 输出、取消、超时与错误处理
```

从 vsh 看，它仍然只是一个普通命令实例，先过 `exec_policy`，再调用实现。vsh 不知道 vbox 的存在，也不另设“原生透传”权限通道。

所有 vbox 支撑命令一视同仁：

- 不设置原生 SSH 名称门；
- 不在 `/usr/bin/git` 的实现中插入 `gitx`；
- 不因为程序名是 `ssh/git/curl/ls/rm` 就改变 exec 或沙箱语义；
- `NoSandbox` 等执行选项使用统一审批与调用契约，不为特定程序增加豁免。

## 3. 配置归属与分发

| 配置 | Pod 分发目标 | 作用 |
| --- | --- | --- |
| `exec_policy` | vsh | **所有命令实例的统一准入** |
| `fs_policy` | vbox 支撑命令，再传给 vbox | 最终原生进程的文件系统沙箱 |
| `net_policy` | vbox 支撑命令，再传给 vbox | 最终原生进程的网络沙箱 |
| `net_policy` | Pod 的 `curl` 命令实现 | 该虚拟命令自身发起的网络请求 |
| `ssh_policy` | Pod 的 `ssh` 命令实现；SSH 族命令共享其 SSH 目标语义 | 虚拟命令自身的 SSH 连接行为 |

配置语法校验与执行能力校验分开：

- 配置无法解析时，错误归属于对应配置域，不能回退为 open；
- 某配置域无效只影响需要该配置的命令，不应把无关命令全部禁用；
- vbox 当前平台无法完整落实某项策略时，只有依赖该能力的 vbox 支撑命令返回“不支持”；
- 能独立落实 `net_policy` 的 `curl` 命令不因 vbox 的能力缺口而被禁止；
- 不允许通过过滤规则、静默降级或切换到另一个命令实现来掩盖错误。

### 3.1 `net_policy` 的两个消费者不是规则分流

`net_policy` 可以同时用于 vbox 支撑命令和 Pod 的 `curl` 命令：

- vbox 支撑命令把完整 net 快照交给 vbox；
- `curl` 在自己的 HTTP 请求路径中落实完整 net 快照；
- 两者是先过同一个 `exec_policy`，再分别执行各自实现；
- 不修改规则行，不按消费者过滤规则，不增加执行层标记。

因此当前配置不接受执行层前缀或字段。策略由哪个实现消费，由 Pod 的命令构造和配置分发决定，而不是由规则语法决定。

### 3.2 `ssh_policy` 只是 SSH 虚拟命令配置

`ssh_policy` 不传给 vbox，也不并入 `net_policy`。即使其规则同样使用 host:port 形式，它仍只描述 Pod 的 SSH 虚拟命令行为。

`grant ssh` 修改的是该虚拟命令的配置快照，不改变任何 vbox 支撑命令的 net 权限。

## 4. 同名命令与绝对路径

普通命令名和绝对路径可以解析为不同命令实例。实例确定后，vsh 对它们使用完全相同的 `exec_policy` 准入与调用接口。

| 调用 | Pod 提供的命令实现 | 命令准入 | 实现内部策略 |
| --- | --- | --- | --- |
| `ssh ...` | Pod `ssh` 命令 | `exec_policy` | `ssh_policy` |
| `/bin/ssh ...` | vbox 支撑的 `/bin/ssh` 命令 | `exec_policy` | vbox `fs_policy + net_policy` |
| `curl ...` | Pod `curl` 命令 | `exec_policy` | `net_policy` |
| `/usr/bin/curl ...` | vbox 支撑的 `/usr/bin/curl` 命令 | `exec_policy` | vbox `fs_policy + net_policy` |
| `git ...` | Pod `git` 命令（若注册） | `exec_policy` | 由该命令自身契约定义 |
| `/usr/bin/git ...` | vbox 支撑的 `/usr/bin/git` 命令 | `exec_policy` | vbox `fs_policy + net_policy` |

Pod 可以注册与系统程序同名的 `ls/rm/ssh/curl/git` 命令，但这不改变绝对路径等原生程序命令的统一模型。

通过 `/bin/ssh` 得到 vbox 支撑命令不是权限绕过，而是明确的命令解析结果。`ssh_policy` 不是对 `/bin/ssh` 的全局约束，Pod 的同名虚拟命令也不是所有原生程序的安全边界。

不同命令实例之间不做隐式失败回退：虚拟命令失败后不改走原生程序，原生程序失败后也不改走虚拟命令。

## 5. 命令实现边界

### 5.1 SSH 族

`sshx` 用于实现 Pod 注册的 `ssh/scp/sftp` 命令：

1. vsh 先按 `exec_policy` 准予调用；
2. 命令实现解析并冻结 SSH 目标；
3. 按 `ssh_policy` 判断目标；
4. 文件传输按自身 `inv.FS` 契约处理；
5. 认证、主机密钥、选项限制、传输和取消由命令实现负责。

内部使用系统 OpenSSH 或 `NoSandbox` 是实现细节，不改变外层统一准入，也不使其成为原生 SSH 的全局代理。

详见 [托管 SSH](../guides/managed-ssh.md)。

### 5.2 curl

`curl` 是 Pod 注册的虚拟命令：

1. 先通过 `exec_policy`；
2. 在自身请求路径中使用 `net_policy`；
3. 文件读写按命令自身的 FS 契约处理；
4. 不把 vbox 的平台能力当成自身能力，也不替 vbox 支撑命令落实网络沙箱。

### 5.3 git

`gitx` 的去留由 Pod 是否注册对应 `git` 命令决定：

- 若提供，它只能作为显式 `git` 命令实现；
- `/usr/bin/git` 等 vbox 支撑命令不得被 `gitx` 拦截；
- 原生 Git hooks 继承其 vbox 沙箱，与 Pod `git` 命令的内部语义无关。

详见 [托管 git](../guides/managed-git.md)。

## 6. 审批与策略是正交关系

审批、命令准入和命令内部策略回答不同问题：

- `exec_policy`：是否允许调用这个命令实例；
- `grant_approved`：是否允许本次脚本修改授权或使用显式 `NoSandbox`；
- `fs_policy/net_policy/ssh_policy`：命令实现获得调用权后，本次资源操作是否允许。

一次整单审批不替代 `exec_policy`，`exec_policy` 通过也不替代资源策略。所有注册命令都经过 exec 准入；需要修改授权的 `grant` 仍额外检查审批事实。

## 7. 模块职责

| 模块 | 现行职责 |
| --- | --- |
| `cli / desktop` | 入口、设备设置、Go 后端生命周期 |
| `cfg` | 配置读取、校验、修改、原子保存和设备设置面 |
| `libs/host` | 总装配、身份、权限快照、命令注册、vbox 支撑命令和调度 |
| `libs/execution` | vsh 装配、日志、前台等待、后台任务和运行记录 |
| `libs/sshx` | Pod SSH 族命令实现，使用 `ssh_policy` |
| `libs/gitx` | 可选的 Pod `git` 命令实现，不拦截 vbox 支撑命令 |
| `libs/mcpx` | MCP 服务连接管理 |
| `libs/vtools` | 将服务登记为 vsh 命令；命令同样经过 exec 准入 |
| `libs/rtc` | hosts_rtc/3 传输，不做业务分发 |
| `libs/hostfs / fsx` | 文件数据面与公共文件操作 |
| `vsh` | 虚拟 shell 与统一 exec 准入 |
| `vbox` | 原生进程 FS/NET 沙箱与进程托管 |
| `protocol` | 传输无关的请求、响应、身份、FS 和信令契约 |

## 8. 目标验收

- 所有注册命令都经过同一个 `exec_policy` 准入点，拒绝时实现未调用。
- `ssh` 与 `/bin/ssh` 可以解析为不同实现，但 exec 决策结构完全相同。
- `/bin/ssh` 不读取 `ssh_policy`，也不被重定向到 `sshx`。
- `/usr/bin/git` 不被 `gitx` 拦截。
- `curl` 使用 `net_policy`，vbox 支撑命令使用 FS/NET 快照，二者不通过 `tool:` 分流。
- 注册 `ls/rm/ssh/curl/git` 后，绝对路径命令仍按自己的注册实现执行。
- vbox 无法落实策略时明确报“不支持”，进程未启动，且不影响无关命令。
- 不存在 `tool:`、`ToolOnly`、原生 SSH 名称门或原生 Git 专属拦截。
