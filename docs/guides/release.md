# 构建与发行边界

技能内容在 **aic-skills** 维护：aic 负责云端发布与内建定版，aic-pod 经同一份 Go embed
消费设备侧 `cli/` 清单与程序源码。Pod 不只是原生 exec/fs 与 MCP manager；它还包含 vtools
服务注册（MCP/TCP）、内置服务清单和 RTC 流装配。

| 产物 | 构建入口 | 包含内容 |
| --- | --- | --- |
| Pod CLI | `make build`、`make cli-<os>-<arch>` | 原生执行、FS、vtools/MCP/TCP 服务注册、内嵌技能 cli 清单与源码、设备传输 |
| Desktop | `make desktop-<os>-<arch>` | Go 后端、Electron、固定版本上游运行依赖 |
| 云端技能 | aic-skills 静态 embed 与 cloud 发布流程 | SKILL.md、UI、API、表定义、cli 清单与源码、普通资源 |

## 内置服务与运行依赖

默认 `browser` 启动 agent-browser 0.38.2 的 `mcp --tools core,tabs,mobile,state`，
`cua` 启动 cua-driver 0.33.2 的 `mcp` 模式。服务首次调用懒启动并共享 SDK session；
内置两项默认 30m `idle_timeout`，到期回收。工具直接来自上游，不存在自研 server、工具重命名或兼容代理。
所有者的同名 `mcp.servers` 配置完整替换默认项（含默认有效期、权限与流声明），可用 `disabled: true` 禁用。

Blender 是另一条 `vtools/tcp` 链：addon 源码来自 aic-skills embed，默认端点
`tcp://127.0.0.1:9866`，包含 doctor/launch/install 生命周期动词，不经过 MCP manager。

Desktop 构建同步资源：

- `desktop/agent-browser.json` 固定官方原生二进制及各平台 SHA-256；无 Browser 专用 Node 运行时。
- `desktop/browser.json` 固定 Chrome for Testing；`desktop/cua.json` 固定上游 CuaDriver 与 SHA-256。
- `make runtime-sync` 同步当前平台开发资源。安装包包含 `resources/agent-browser`、`resources/browser`、`resources/cua`；运行时无需下载软件。
- 独立 CLI/Docker 的 Go 构建不包含这些运行依赖，用户按技能说明安装上游程序，或用 `AIC_AGENT_BROWSER_PATH`、`AIC_BROWSER_PATH`、`AIC_CUA_DRIVER_PATH` 指定。没有通用用户技能包安装器。

macOS 按上游要求将随包 CuaDriver.app 安装到 `/Applications/CuaDriver.app`，官方 MCP 再按应用名启动它；不增加平台安装器或启动代理。CuaDriver 保留上游 app 身份，桌面权限由系统授予该 app。驱动 daemon 生命周期由上游负责；升级正在运行的驱动后，用官方 `cua-driver stop` 停止旧 daemon，再启动新 app。Pod 关闭时回收自己的 MCP 连接与子进程。

## 技能内容的两个版本面与两条生效链

**模块版本**（aic-skills `VERSION` / git tag，供 aic 与 aic-pod 的 Go `require` 消费）与
**技能版本**（各包 `SKILL.md` frontmatter，供内建定版指纹消费）不是一回事。当前版本不要在本文
复制，统一查 [aic-skills README 的版本表](../../../aic-skills/README.md)。

- **SKILL.md / UI / API / 文档等内容**：先 bump 对应技能版本并发布 aic-skills 模块；再由 aic
  升级 `require`、重建并重启后端。`skills.builtin` 维护事件按「包名 + 内嵌版本」指纹自动定版，
  管理员也可强制重跑；执行时整包覆盖，不做行版本大小比较。
- **`cli/manifest.json` / addon / 设备程序源码**：同样必须发布 aic-skills 模块；再由 aic-pod
  升级 `require`、重建后端或 Desktop 并重启，新的服务清单与源码才会进入设备。Browser 若改
  `stream.channel`，前端也必须显式使用同一标签。
- 本地根 `go.work` 会用兄弟目录掩盖依赖差，**不能代替发行验证**。发布前必须按下方
  `GOWORK=off` 命令验证各消费方能按已发布模块独立构建。

Browser UI 经 RTC 转发 agent-browser 原生帧流和键鼠输入，复用上游 daemon 的同一 CDP 连接，
绘制后回传 ACK。关闭 UI 不关闭浏览器（画面挂载期间刷新空闲截止时间）；Pod 关闭时、以及内置
browser 空闲到期时，用上游 CLI 清理自己的 daemon。实时通道名来自 browser manifest；所有者
覆盖默认服务时不继承内置流。

## 验证命令

```sh
GOWORK=off go mod tidy
GOWORK=off go build ./...
GOWORK=off go test ./...
go test -race ./libs/vtools ./libs/host
go test ./libs/rtc ./protocol
make build backend-bin
# 上游依赖已安装或由 AIC_* 环境变量指向本地 bundle：
AIC_MCP_TEST=1 go test ./libs/host -run TestUpstreamMCPThroughExec -count=1
cd desktop && npm test
```

实机集成验证工具元数据透传、共享浏览器的人工点击/中文输入、正式服务命令、快照、截图、
RTC 通道与 ACK 限流、断开观看及 Pod 关闭清理。跨平台 Go 编译不等价于 Windows/Linux 桌面权限
与驱动实机验证。
