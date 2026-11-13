# Host vtools：MCP 服务注册与调用

> 现行契约。vtools 只是 MCP Provider 的调用内核；命令注册统一归 command hub。
> 远程 MCP 端点只接受 `http/https` URL。

## 1. 调用模型

每个 MCP 服务 alias 登记一个 vsh 根命令：

```text
<alias>                              工具目录（含每个工具的 flag 摘要）
<alias> <tool> --help                按 inputSchema 渲染参数表
<alias> <tool> [--flag value]...     标量参数按 inputSchema 映射
<alias> <tool> --input JSON|-        复杂 JSON 参数（对象/数组）
<alias> <tool> [--json] [--image-preview WIDTHxHEIGHT]
```

### 1.1 参数来源与优先级

同名参数优先级（高→低）：flag 显式值 > `--input` JSON 对象的同名键 > schema default
（只在参数表展示，不在客户端下发）。缺必填参数：客户端已取到 schema 时退 2
（`missing required argument --x`）；纯 `--input` 直达不额外往返，交给服务端校验。

### 1.2 flag 映射规则

| schema type | flag | 取值 |
| --- | --- | --- |
| string | `--s v` / `--s=v` | 原样不 trim；空串写 `--s=`；裸 `--s` 报错 |
| boolean | `--b` / `--b=v` | 布尔解析；裸写即 true |
| integer | `--i v` | 十进制整数 |
| number | `--n v` | 浮点数 |
| object / array | 不可映射 | 只能走 `--input` |

`type` 是数组时取第一个非 `null` 项。enum 越界退 2（`is not one of ...`）。
命令面保留名 `json` / `input` / `image-preview` / `help` / `h` 遮蔽同名工具参数：
这些参数只能走 `--input`（参数表会标注）。类型错与未知 flag 的报错都附带该工具
**可映射的 flag 清单**。

### 1.3 值来源：stdin

`--<flag> -`（空格形式）表示该参数的值从 stdin 读；`--<flag>=-`（等号形式）表示字面量 `-`。
stdin 只能被消费一次：`--input -` 与 `--flag -` 不能同时用（退 2）。string 去掉末尾换行，
数值/布尔去空白。

```sh
cat strategy.lua | pbz strategy_set --phase exploring --code -
pbz strategy_set --phase exploring --code - < strategy.lua
```

长文本（代码、配置、prompt）因此不必手拼 JSON，也不必嵌套 shell 转义。

### 1.4 目录与参数表

`<alias>` 无参输出工具目录，并在每条工具下附带 flag 摘要（名/类型/必填/枚举），非标量属性
单列 `via --input`；`<alias> --json` 输出完整 tools 数组（含 inputSchema）；
`<alias> <tool> --help` 输出该工具的参数表。

### 1.5 退出码

0 成功；1 服务级失败（MCP `isError`、传输失败）；2 参数面错误（未知 flag、类型错、enum 越界、
缺必填、stdin 冲突、用法错）；141 SIGPIPE。

Provider 最小接口仍是 `Tools(ctx)` 与 `Call(ctx, tool, args)`；当前生产适配器只有
`vtools.MCP`。配置热加载由 command hub 重建注册表；未变化的命令保留原 manager/session。
MCP `isError` 映射为 `CallError`，结果照常渲染但命令退出非零。

`--image-preview` 由 MCP 适配器实现，只缩放成功结果中的图片，不改变上游 structuredContent。
MCP ImageContent 统一经 vsh ResultAttrs 传递：单图 `image_data`、多图 `image_datas`，stdout 不再携带图片 base64；图片 content 的位置与元数据保留并标记附件位置。
普通工具调用直接使用服务根命令；不再有调试直通入口。

## 2. 注册与生命周期

- 平台命令先注册，统一命令后登记；撞名命令进入 rejected，不覆盖平台命令。
- `commands` 是设备命令面的唯一入口：无参时列出当前可发现的命令（平台自定义指令 + 已登记服务），带子命令时进入设备管理面（list/info/add/remove/start/stop/restart/config/install/uninstall）——与 cloud/page 同一入口、同一口径。
- 技能包装配失败（包目录缺失、manifest 缺失或非法）只跳过该包：其余命令照常注册，
  坏包在 `commands list` 里单列一行（行名 `package:<skill_id>`、`kind=package`、`state=rejected`、
  `id` 列写 skill_id、`error` 写原因），修好后重装或重装配即恢复。
- 登记是即时的，连接是懒惰的；第一次工具调用才建立 stdio/HTTP MCP session。
- `commands start` 预连接，`commands stop` 断开当前 session，`commands restart` 断开后重建；
  命令登记不随 stop 消失。
- manager 拥有进程与共享 session。单次调用取消只取消该调用，不销毁整个服务。
- 配置或服务重建关闭旧 session；不相关的命令保留原 manager 和流进程。

```sh
commands add <alias> --protocol mcp [--cwd DIR] [--env K=V]... [--idle DUR] [--persist] -- <program> [args...]
commands add <alias> --protocol mcp --url <http(s)-url> [--persist]
commands remove <alias> [--persist]
commands start|stop|restart <alias>
commands list [--json]
commands info <alias> [--schema]
```

owner 手工服务与技能包都落在统一 `commands` 配置/定义面；`--persist` 写
`$HOME/.aic/config.yaml` 的 `commands` 段，否则只进入运行期定义表。删除定义即停用服务，
不再有单独的 `mcp.servers` 或 `disabled` 配置。HTTP 只接受 `http/https`，重定向关闭，
设备 net 规则生效；stdio 进程的 cwd/env/no_sandbox 由 owner 配置。没有 OAuth/token
管理器，也没有其他 URL scheme。

## 3. 权限边界

- `commands add/remove/config` 的管理门是 `commands`。
- 所有统一注册服务的调用门都是 alias 本身，不使用 `mcp.<alias>`。
- `no_sandbox` 只能来自 owner 配置或带人工审批的 `commands config <alias> --no-sandbox`。
- 工具参数不能覆盖 cwd/env/no_sandbox，也不能把 TCP URL 写成 MCP 服务。

## 4. 技能包统一 commands

技能包根 `manifest.json` 使用 `schema/ui/host`。pod 显式忽略 `ui`，只装配 `host`：

- `run + protocol=mcp`：包内或 PATH 程序的 stdio MCP；
- `url + protocol=mcp`：远程 HTTP MCP；
- 普通 `run`：一次性进程；
- `run + streams`：包内常驻进程拥有的 RTC 字节流，标签为 `<alias>:<stream>`。

官方 browser/CUA/Blender 与普通第三方包同形态。browser 是包内 Go CDP 服务；CUA 包装官方
`cua-driver mcp`；Blender 直接运行现成 `uvx blender-mcp`。pod 不包含这些包的专属 Provider、
安装目录或生命周期分支。

browser/cua 由 pod 启动定版预装（发行包附带预编译 `bin/<name>`），其余设备包用
`commands install <id>` 拉取并注册。需要包内程序时源码位于 `src/`；使用现成 MCP 的包不需要自研桥接或额外协议实现。

## 5. RTC 流

RTC 字节流是独立传输面，不是 MCP，也不是 `vtools/tcp` 的替代名称。流只做不透明双向
TCP↔RTC 中继，权限按 alias 复查，页面与包共同拥有应用层帧协议。旧独立 `aic-browser`
通道和包专属流注册已删除，现行标签只取命令注册表中的有效定义。
