# 统一 Commands 与技能设备扩展设计

> 状态：**已落地**（2026-10-31 设计稿，2026-10-31 随统一 commands 机制实施）。本文是设计
> 记录，不是现行契约；现行契约见 `reference/host-vtools.md`（§5 技能包的设备维度）、
> `reference/hosts-tools.md` 与 `guides/browser.md`，技能作者手册见
> `aic-skills/create/references/device-cli.md`。

## 1. 目标与边界

现有 host 扩展同时存在多套模型：官方内嵌技能、第三方手工 MCP 配置、普通脚本、TCP 工具协议、
browser 专用流和 Blender 专用生命周期。目标是统一它们的**注册、发现与基础调用方式**，
不是把命令的应用责任收进 aic-pod。

aic-pod 的定位类似 shell：

- 注册命令，提供根命令入口；
- 发现命令，按名字找到当前定义；
- 转发 argv、stdin/stdout/stderr、退出码或 MCP 调用；
- 对常驻命令提供基础启动、停止、重启与空闲回收；
- 对 stream 提供 TCP 与 RTC 之间的原始字节搬运；
- 在命令调用和流打开时执行入口权限检查。

命令自己负责自己的应用行为，包括依赖与构建说明、应用配置、业务状态、工具目录、应用协议、
版本协商、断线恢复以及多客户端共享策略。平台不为这些行为发明通用字段。

本设计不建立跨 cloud/page/host 的统一注册中心：

- host 的 `commands` 继续发现 host 已有命令；
- page 的 `commands` 继续发现现有 `pageDesc.commands`；
- cloud 的技能发现继续走 `skills`；
- page 只新增通用 ByteStream 客户端，不改变 page 命令模型；
- `ui/**`、`api/**`、`tables/**` 继续路径即协议。

## 2. 核心结论

1. 云端只有 `skill`，负责技能包发现、加载、发布、fork 与下载；设备侧不新增 `skill` 命令。
2. 统一的是 host 扩展命令的装配路径。普通 CLI、脚本、MCP、官方技能和第三方技能都注册为根命令。
3. 官方技能与第三方技能完全等价；官方技能只是预装技能包。
4. MCP 是一种命令调用协议，不是插件系统中心。
5. 请求响应使用普通进程或 MCP；实时数据使用不透明双向 TCP 字节流。
6. 命令名只是索引。命令定义及其配置属于具体来源，同名替换不继承配置。
7. 删除平台通用 `vtools/tcp v1` 工具协议，不提供新旧协议兼容层。
8. 技能包根 `manifest.json` 只声明设备执行内容。
9. 官方 browser 包是统一机制的验收用例，不为它保留 pod 特判。

## 3. 命令定义与名字索引

### 3.1 定义身份

每个 host 命令定义有稳定身份：

```text
平台定义：builtin:<name>
技能定义：skill:<skill_id>:<manifest_command_name>
配置定义：config:<config_command_name>
```

定义身份用于保存来源、owner 策略和 stream 接线。它不等于调用名。

调用方看到的是根命令名，例如：

```bash
browser cdp --input -
hello
blender get_scene_info
```

host 保存一张名字索引：

```text
command_name -> 当前有效的命令定义身份
```

名字只是查找键。env、cwd、免沙箱姿态、stream 接线等都属于定义，不属于名字。

### 3.2 同名解析

host 命令来源优先级从高到低固定为：

1. 平台虚拟指令；
2. 技能包指令；
3. 设备 `commands` 配置。

规则：

- 高优先级定义生效时，低优先级同名定义仍保留，但不覆盖它，也不报来源冲突；
- 同一级别内，后注册覆盖先注册；
- 官方技能与第三方技能同属技能级，后安装的第三方同名命令可以生效；
- 注册顺序持久化，重启与热加载按同一顺序重建；update 不改变原注册顺序；
- 高优先级定义被移除后，名字索引重新指向仍存在的次优先级定义；
- 不保留 override/local 两套命令配置。

例如：

```text
A = skill:package-a:browser，no_sandbox=true
B = skill:package-b:browser，no_sandbox=false

B 后注册并覆盖 A：
  browser -> B
  使用 B 的定义和策略，不继承 A 的 no_sandbox、env 或 stream

B 被移除：
  browser -> A
  恢复使用 A 自己保存的定义和策略
```

page 与 cloud 不参与这张 host 名字索引，不同端原有同名命令互不影响。

## 4. 技能包结构

```text
SKILL.md         必需：包元数据与 AI/用户手册
manifest.json    可选：设备执行声明；没有它 = 无设备命令
ui/**            可选：页面与包自己的 viewer，路径即协议
api/**           可选：云端接口，路径即协议
tables/**        可选：数据表，路径即协议
src/**           可选：设备程序源码
scripts/**       可选：脚本
```

职责分离：

- `SKILL.md` 保存 name、version、description 等包身份，manifest 不重复；
- `ui/**`、`api/**`、`tables/**` 由平台按现有目录协议消费，不在 manifest 再声明；
- `manifest.json` 只给 host 装配使用；
- viewer 是包内 UI 普通源码，平台不提供共享 viewer，也不理解包的帧与输入语义；
- 云端包只保存源码与脚本，不保存构建产物、二进制、密钥或用户数据。

## 5. 根 manifest.json

### 5.1 完整形态

```json
{
  "schema": 1,
  "commands": {
    "hello": {
      "run": ["python3", "scripts/hello.py"]
    },
    "browser": {
      "run": ["bin/browser"],
      "protocol": "mcp",
      "idle_timeout": "30m"
    },
    "remote-tools": {
      "url": "http://127.0.0.1:9000/mcp",
      "protocol": "mcp"
    },
    "raw-view": {
      "run": ["bin/raw-view"],
      "idle_timeout": "30m"
    }
  },
  "streams": {
    "view": {
      "command": "raw-view",
      "address_env": "RAW_VIEW_STREAM_ADDR"
    }
  }
}
```

### 5.2 `schema`

manifest 结构版本，与 `SKILL.md` 中的技能版本无关。未知 schema 直接拒绝安装或更新。

### 5.3 `commands`

map key 是包内命令名，也是默认的根命令名。支持三种基础执行形式：

| 声明 | 含义 |
| --- | --- |
| `run`，无 protocol，且未被 stream 引用 | 普通一次性进程 |
| `run + protocol=mcp` | pod 托管的 stdio MCP 常驻进程 |
| `url + protocol=mcp` | pod 连接的 HTTP MCP 服务 |

另有一条派生规则：

| 声明 | 含义 |
| --- | --- |
| 被 `streams` 引用的 `run` | 常驻单例 stream 提供进程，不要求 MCP |

约束：

- `run` 是 argv 数组，不经过 shell；
- `run` 与 `url` 互斥；
- 当前唯一 protocol 是 `mcp`，缺省表示普通进程；
- 包内相对路径以安装后的包根目录为基准，PATH 程序名可直接使用；
- 包内命令工作目录固定为包根目录；
- `idle_timeout` 只用于常驻进程，普通一次性命令声明它属于配置错误；
- manifest 不包含 `no_sandbox`、权限申请、构建脚本或应用配置。

### 5.4 普通进程调用

- 调用参数追加到 `run` 后；
- stdin/stdout/stderr 直通；
- 退出码原样返回；
- 每次调用独立启动进程；
- 不需要工具目录、inputSchema 或 MCP。

### 5.5 MCP 调用

- stdio 进程由 pod 懒启动、复用和按 idle 回收；
- HTTP 形态只管理 pod 侧客户端，不声称拥有远端进程；
- 工具目录、inputSchema、structuredContent 与 isError 由 MCP 提供；
- 注册为 `<alias> <method> [--flag value]... [--input JSON|-] [--json]`。

MCP method 的应用行为由命令自己实现。pod 不理解 browser、Blender 或其他命令的业务语义。

### 5.6 stream-only 调用

被 stream 引用的普通 `run` 是常驻服务，不再同时承担一次性根命令调用：

- 打开包内 stream 可以触发它懒启动；
- start/stop/restart 管理这个单例进程；
- 直接执行其 alias 时明确报告“该命令仅提供 stream”；
- 它不需要声明 MCP，也不需要提供请求响应工具。

### 5.7 源码与构建

平台不发明通用构建系统，不自动执行包内 install/build 脚本。

- 第三方按 `SKILL.md` 在设备上安装依赖并编译到 manifest 约定位置；
- 官方技能可以随桌面端预装或预编译，但装配时使用同一 manifest 与同一路径规则；
- 缺少可执行文件时明确报告“尚未按 SKILL.md 完成构建”，不静默退回其他程序；
- 安装、构建、授权、启动是不同动作。

应用需要设备专属配置时，由命令按自己的 SKILL.md 读取自己的配置文件或环境。
平台不发明应用配置中心：应用配置走**命令定义级配置**（见 §6），
不新增 alias override 或第二套配置语义。

## 6. 设备 commands 配置与命令定义级配置

设备所有者可以手工注册自包含命令：

```yaml
commands:
  hello:
    run: ["python3", "/abs/path/hello.py"]

  browser:
    run: ["/abs/path/browser", "--chrome-arg=foo"]
    protocol: "mcp"
    idle_timeout: "30m"
    cwd: "/abs/path"
    env:
      BROWSER_SECURE_ORIGINS: "http://www.ideas.cn"
    no_sandbox: true
```

`cwd`、`env`、`no_sandbox` 都属于这条配置定义。它们不会叠加到同名技能命令上。

### 6.1 命令定义级配置（统一面）

**一张表装两件事**：应用配置（env）与沙箱策略（no_sandbox）——它们同属
「这条命令定义的运行配置」。存储键是**定义身份**，不是可被同名替换的 alias：

```yaml
command_config:
  skill:browser:browser:
    env:
      AIC_BROWSER_PATH: "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
    no_sandbox: true
```

写面只有一个命令（运行期生效，不需重启后端）：

```bash
commands config <alias>                     # 显示当前配置
commands config <alias> K=V ...             # 设置应用配置（env）
commands config <alias> --unset K           # 删除一个环境变量
commands config <alias> --no-sandbox        # 免沙箱运行（提权项）
commands config <alias> --sandbox           # 取消免沙箱
commands config <alias> --clear             # 清空该定义的全部配置
commands config <alias> --json              # 以 JSON 输出
```

信任级：

- **env**：AI 会话可直接写（只是环境变量，不越权）；
- **no_sandbox**：逃出 pod 沙箱 = 提权。来自平台的会话必须带平台审批事实
  （平台侧人工批准通道），否则明确拒绝并提示；设备 owner 在本机直接执行
  同一命令不需要审批。

生命周期：

- 同名其他技能或配置命令不继承它（配置跟定义身份走）；
- 该包 update 时命令身份不变，配置继续属于该命令；
- 该包 uninstall 后，其配置随定义失效；
- 配置文件里的同名段是 owner 直接改的等价物，热加载生效。

`commands` 与 `command_config` 支持热加载。它们只负责命令运行，不扩大 exec/fs/net/ssh 权限域。
旧 `mcp.servers` 不兼容读取。

## 7. host commands 命令面

### 7.1 发现

```bash
commands
commands list [--json]
commands info <name>
commands info <name> --schema
```

无参数 `commands` 继续用于 host 指令发现。输出至少包含 name、执行形式、source 与运行状态。
`list` 默认展示最终生效命令；`info` 展示当前定义身份、来源和运行错误。

不为 page/cloud 增加 `--target` 或统一路由。

### 7.2 技能设备安装

```bash
commands install <skill_id>
commands update <skill_id>
commands uninstall <skill_id>
```

这些操作只负责设备包的获取、移除与注册刷新，不加载 AI 手册、不修改云端技能关系，
也不自动构建源码。运行期更新注册表，不要求重启 aic-pod 或 aic。

取包面：走设备已授权的 `/fs/cloud` 文件面（包目录即 `/skills/{id}`），不是
`/api/skills/{id}/download`——设备凭证是静态 key，`/api` 全树由 RequireLogin
挡住（只认会话凭证），静态 key 进不去。私有包的可见性由平台 skill 门判定
（owner 读写、非 owner 恒拒），ACL 与技能面同口径。

### 7.3 手工命令

普通进程：

```bash
commands add hello -- python3 /abs/path/hello.py
```

stdio MCP：

```bash
commands add browser --protocol mcp -- /abs/path/browser --chrome-arg=foo
```

HTTP MCP：

```bash
commands add remote-tools --protocol mcp --url http://127.0.0.1:9000/mcp
```

删除：

```bash
commands remove <alias>
```

规则：

- `--` 后全部属于子进程 argv；
- 默认仅运行期生效，`--persist` 写入设备 `commands` 配置；
- `remove` 删除手工定义，技能命令通过 `uninstall` 按包移除；
- 手工命令仍属于配置级，不覆盖技能命令；
- 手工定义被技能同名命令遮蔽时，其配置仍保留，但不参与当前调用。

### 7.4 基础生命周期

```bash
commands start <alias>
commands stop <alias>
commands restart <alias>
```

适用于 stdio MCP、stream 提供进程和 HTTP MCP 客户端：

- start = 预启动或预连接；
- stop = 停止当前实例，不删除定义；后续调用或打开流仍可再次懒启动；
- restart = 停止并重新启动当前实例。

对一次性进程调用时明确报“不支持”。应用级生命周期，例如 `blender launch`，由命令自己的
MCP method 或普通子命令实现，不扩展平台生命周期模型。

### 7.5 MCP 调试

```bash
commands mcp <alias> tools
commands mcp <alias> describe <method>
commands mcp <alias> call <method> --input -
commands mcp <alias> read <resource-uri>
```

正常调用不经过调试面：

```bash
browser
browser cdp --input -
browser cdp --help
blender get_scene_info
hello
```

旧根 `mcp` 管理命令删除，不保留别名。

## 8. 安装与注册

安装流程：

```text
commands install <skill_id>
        │
        ├─ GET /fs/cloud/skills/{skill_id}?depth=5（设备已授权的文件面）
        ├─ 逐文件安全落到设备包目录（无 zip 中转）
        ├─ 读取并校验根 manifest.json
        ├─ 注册命令定义、名字索引与 stream 接线
        └─ 运行期立即可发现
```

要求：

- 公开包与调用者私有包复用平台 skill 门的可见性判定，不另造云端协议；
- 逐文件落地拒绝绝对路径、`..` 穿越与超限内容（单文件 32 MiB、4096 文件、总量 128 MiB）；
- 无 manifest 的包不需要设备安装，云端手册/UI/API 仍可使用；
- update 只获取新包并重新装配，不自动构建；设备侧已编译的 `bin/` 构建产物跨更新保留（源码变了要自行重建）；
- uninstall 移除该包拥有的定义、owner 策略与进程，再重新解析名字索引；
- 官方预装包与第三方包只区别于包文件来源，注册、权限和生命周期完全相同；
- 官方预装包的 uninstall 状态持久化，重启后不悄悄恢复。

平台只保证注册与基础进程语义。更新后的依赖安装、构建、应用数据迁移和版本协商由命令自己负责。

## 9. 权限

| 操作 | 权限 |
| --- | --- |
| `commands list/info` | 按平台现有发现面开放 |
| install/update/uninstall/add/remove | `grant cmd commands` |
| 调用 `<alias>` | `grant cmd <alias>` |
| start/stop/restart `<alias>` | `<alias>` 调用权限 |
| 打开 `<alias>` 当前定义拥有的 stream | `<alias>` 调用权限 |
| `commands mcp <alias>` | `<alias>` 调用权限 |

删除权限名中的实现细节：

```text
mcp.browser   → browser
process.hello  → hello
skill.browser → browser
```

调用权限针对当前根命令入口；owner 运行策略针对具体定义身份。两者都不把某个定义的 env、
免沙箱姿态或 stream 接线复制给同名定义。

## 10. 统一双向流

### 10.1 manifest 只声明接线

```json
"streams": {
  "view": {
    "command": "raw-view",
    "address_env": "RAW_VIEW_STREAM_ADDR"
  }
}
```

含义：

1. `view` 是包内流名；
2. `command` 只能引用同一个 manifest 中的命令定义；
3. pod 分配回环地址，并在启动该进程前写入 `address_env`；
4. 命令在该地址监听 TCP，pod 连接后双向搬运原始字节；
5. `url` 命令没有可注入地址的托管进程，不能这样声明；
6. stream 只属于其声明定义，不跟随 alias 转到同名其他定义。

当命令定义被同名定义遮蔽时，它的 stream 也不可通过当前 alias 打开。移除遮蔽定义后，
原命令与自己的 stream 一起恢复。

全局 RTC label 由平台派生——**流与命令同属一个命名空间**：

```text
<command_alias>:<stream_name>
```

label 里没有包 id：命令叫什么，流就叫什么（同名命令按注册表的生效定义解析，
与 shell 里同名命令的遮蔽规则一致）。包不声明全局 channel。

### 10.2 平台协议只到 TCP

```text
包内 UI
   │ Uint8Array
   ▼
RTC DataChannel（只负责传输与分片）
   │
   ▼
aic-pod（不解析载荷）
   │ TCP 原始字节
   ▼
包自己的进程
```

平台不定义 JSON/二进制、帧头、JPEG/PNG、输入事件、ACK、帧率、应用版本握手、
多 viewer 共享状态或断线恢复。这些由同一个包的服务与 UI 自己协商。

TCP 不承诺应用消息边界。包自己的协议必须处理拆包与粘包；RTC 分片只是传输细节。

### 10.3 基础传输与进程生命周期

- 有序、可靠 RTC；
- 有界输入/输出队列与背压，禁止无界读取和堆积；
- 每次打开流建立独立 TCP 连接，连接之上的应用会话由命令自己决定；
- UI 关闭关闭对应 TCP，服务关闭关闭对应 UI 流；
- 活跃流刷新所属进程 idle 时间；
- 不增加平台 ACK，不重放应用输入；
- 打开流可以触发所属常驻进程懒启动，连接就绪超时明确报错。

## 11. 前端 ByteStream

将 browser 专用 `BrowserStream` 改为通用二进制流客户端：

```js
const conn = await $hosts.openTools(hostId)

const stream = conn.openStream({
  skill_id: $mod.params.skill_id,
  name: "view",
  onData(bytes) {
    // bytes: Uint8Array，由本包 viewer 解码
  },
  onClose(error) {}
})

stream.send(new Uint8Array([1, 2, 3]))
stream.close()
```

文本也由包自己编码：

```js
stream.send(new TextEncoder().encode(JSON.stringify({type: "click", x: 10})))
```

平台不再强制 `JSON.parse`，不要求入方向必须是 string，也不提供共享 viewer。
ByteStream 只提供基础传输，不承担包内协议的版本兼容、重连或状态恢复。

## 12. MCP 与旧 TCP 的收口

MCP 保留为命令适配协议：

- stdio：`run + protocol=mcp`；
- HTTP：`url + protocol=mcp`；
- 管理面统一进 host `commands`；
- 原始调试统一进 `commands mcp`；
- 内部实现包可以继续叫 `mcpx`，公开契约不再以 MCP 为中心。

删除平台通用 `vtools/tcp v1`：

- `mcp add --tcp` / 新 `commands add --tcp` 不提供；
- manifest 不存在 `kind: tcp`；
- 删除 TCPProvider 作为平台工具协议适配器；
- 不做 TCP 自动探测、TCP→MCP 转换、双栈或旧配置回落。

请求响应工具不得借用不透明 stream 模拟平台工具协议，应使用普通进程或 MCP。

## 13. Blender 迁移

Blender 需要区分平台协议与包内协议。

### 13.1 平台边界

目标结构：

```text
pod
  │ stdio MCP
  ▼
blender-bridge（技能包 src/ 编译）
  │ 包内私有 TCP 协议
  ▼
Blender addon 127.0.0.1:9866
  │
  ▼
Blender Python / bpy
```

平台只理解 stdio MCP。9866 可继续作为 Blender 包服务与 addon 的内部协议，不再是设备插件契约，
因此不属于平台兼容层。

### 13.2 manifest

```json
{
  "schema": 1,
  "commands": {
    "blender": {
      "run": ["bin/blender-bridge"],
      "protocol": "mcp",
      "idle_timeout": "30m"
    }
  }
}
```

现有 `doctor/launch/install` 与场景工具全部成为 bridge 暴露的 MCP method：

```bash
blender doctor
blender launch
blender install
blender get_scene_info
blender execute_code --input -
```

pod 中 Blender 专用的安装、启动、token、地址、错误翻译与自定义命令代码移入技能包。
`lifecycle`、`verbs`、`RegisterCustom` 等专用契约随之取消。

Blender 当前截图继续作为 MCP 图片结果，不虚构 RTC 流。

## 14. 明确删除的旧设计

- pod 版 `skill` 管理命令；
- 独立 `tool` 管理命令；
- 根 `mcp` 管理命令与 `mcp.servers` 配置；
- `cli/manifest.json` 与 `service.kind`、`programs`、`lifecycle`、`verbs`；
- `mcp-stdio | mcp-http | tcp` 插件类型体系；
- 平台通用 `vtools/tcp v1` 请求响应协议；
- `mcp.<alias>` 权限名；
- 按 alias 保存或继承的 `no_sandbox_commands`；
- 跨 cloud/page/host 的统一 Registry 与 `commands --target`；
- browser WebSocket 特判与 BrowserStream JSON 协议；
- 平台共享 viewer、应用帧格式、输入协议与 ACK 设计；
- manifest 中的 `ui/api/tables`、包身份、权限姿态、应用配置与全局 stream channel。

不提供旧配置回落或新旧协议自动兼容。

## 15. 实施顺序

### P0：基础命令模型

- 定义身份、名字索引、来源优先级与 host 注册表；
- 根 manifest 解析与普通进程命令；
- `commands list/info/add/remove/start/stop/restart/mcp`；
- 设备 `commands` 配置、定义级 owner 策略与热加载。

### P1：技能安装

- 下载、安全解包、安装状态与 update/uninstall；
- 官方预装和第三方安装共用装配链；
- 权限名统一为 `<alias>`。

### P2：通用流

- stream-only 常驻进程、manifest streams 与端口注入；
- TCP relay、RTC label 派生、背压与权限门；
- 前端 ByteStream。

### P3：旧实现迁移

- 删除 vtools/tcp 与 browser 特判；
- Blender bridge 迁入技能包；
- 官方 browser 包按统一机制完成闭环，不为它在 pod 里留特判。

## 16. 验收

### 16.1 普通进程

1. 私有包只声明 `hello.run`；
2. `commands install <skill_id>` 后立即出现 `hello`；
3. argv、stdin、stdout、stderr、退出码与工作目录符合契约；
4. 不需要 MCP，不重启 pod。

### 16.2 stream-only

1. 私有包只声明一个普通 `run` 和引用它的 stream，不声明 MCP；
2. 打开包内 UI 的流后，进程懒启动并监听注入地址；
3. UI 与服务使用自定义二进制协议完成双向通信；
4. start/stop/restart 与 idle 回收符合基础生命周期；
5. pod 不解析应用数据，也不需要 MCP 工具目录。

### 16.3 同名定义隔离

1. A、B 两个包声明同名命令，但使用不同 env、owner 策略和 stream；
2. B 后注册生效时，只使用 B 的定义，不继承 A 的任何配置；
3. B 的 stream 不会接到 A 的进程，A 的 stream 也不会转接到 B；
4. 移除 B 后，A 与自己的配置和 stream 一起恢复；
5. 重启与热加载后，同名解析结果不变。

### 16.4 browser

1. 创建私有包，内容只有 SKILL.md、manifest、src 与 ui；
2. `commands install <skill_id>`，不手工 `mcp add`；
3. 按 SKILL.md 编译源码，并由 browser 自己读取设备专属应用配置；
4. owner 在终端为该技能定义配置 `no_sandbox`，热加载生效；
5. `grant cmd browser` 后调用 CDP 工具；
6. 包内 UI 打开 `view`，服务与 viewer 使用自己的协议；
7. 实时画面与人工输入通过，多 viewer 和断线恢复由包自己的协议验收；
8. 将包命令改为 `browser` 后，同名替换官方技能不需要修改 pod，也不继承官方定义的配置。

最后一条若仍需要任何 `if alias == "browser"`，说明统一机制未完成。

### 16.5 现有发现面不变

1. page `commands` 仍只列出现有 `pageDesc.commands`；
2. host `commands` 列出 host 当前有效命令；
3. cloud 技能仍经 `skills` 发现；
4. 不新增跨端命令路由或统一管理面。

### 16.6 Blender

1. pod 不再包含 Blender 专用 Provider 或生命周期分支；
2. `blender doctor/launch/install` 与既有工具经同一 MCP 命令面工作；
3. 删除 vtools/tcp 后 Blender 功能完整；
4. addon 的 9866 私有协议不会泄漏到 manifest、权限或其他技能。

## 17. 非目标

- 不统一 cloud/page/host 的命令注册中心；
- 不自动构建来源不明的源码；
- 不设计通用包构建系统或应用配置中心；
- 不让包通过 manifest 自己获得权限；
- 不把命令应用配置合并到 alias 或同名定义；
- 不统一不同端不支持的生命周期操作；
- 不理解或规范包内 stream 载荷；
- 不负责包内协议的版本协商、应用重连或多 viewer 会话；
- 不提供共享 viewer；
- 不保留旧协议兼容层；
- 不因为 browser 的个别需求给 pod 增加专用字段或分支。
