# Host vtools：MCP 服务注册与调用

> 现行契约。vtools 只是 MCP Provider 的调用内核；命令注册统一归 command hub。
> 远程 MCP 端点只接受 `http/https` URL。

## 1. 调用模型

每个 MCP 服务 alias 登记一个 vsh 根命令：

```text
<alias>                              工具目录
<alias> <tool> [--flag value]...     标量参数按 inputSchema 映射
<alias> <tool> --input JSON|-        复杂 JSON 参数
<alias> <tool> --json                全量结构化结果
<alias> <tool> --help                上游参数表
```

Provider 最小接口仍是 `Tools(ctx)` 与 `Call(ctx, tool, args)`；当前生产适配器只有
`vtools.MCP`。配置热加载由 command hub 重建注册表；未变化的命令保留原 manager/session。
MCP `isError` 映射为 `CallError`，结果照常渲染但命令退出非零。

`--image-preview` 由 MCP 适配器实现，只缩放成功结果中的图片，不改变上游 structuredContent。
普通工具调用直接使用服务根命令；不再有调试直通入口。

## 2. 注册与生命周期

- 平台命令先注册，统一命令后登记；撞名命令进入 rejected，不覆盖平台命令。
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
