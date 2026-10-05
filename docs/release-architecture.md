# 构建与发行边界

技能内容在 cloud 发布；Pod 只保留原生 exec/fs 与 MCP manager。browser/CUA 直接使用上游独立程序。

| 产物 | 构建入口 | 包含内容 |
| --- | --- | --- |
| Pod CLI | `make build`、`make cli-<os>-<arch>` | 原生执行、FS、MCP manager、默认启动配置与设备传输 |
| Desktop | `make desktop-<os>-<arch>` | Go 后端、Electron、固定版本上游运行依赖 |
| 云端技能 | aic-skills 静态 embed 与 cloud 发布流程 | SKILL.md、UI、API、表定义、普通资源 |

默认 `browser` 启动 agent-browser 0.38.2，`cua` 启动 cua-driver 0.33.2 的 `mcp` 模式。服务首次调用懒启动并共享 SDK session。工具直接来自上游，不存在自研 server、工具重命名或兼容代理。所有者的同名 `mcp.servers` 配置完整替换默认项，可用 `disabled: true` 禁用。

Desktop 构建同步资源：

- `desktop/agent-browser.json` 固定官方原生二进制及各平台 SHA-256；无 Browser 专用 Node 运行时。
- `desktop/browser.json` 固定 Chrome for Testing；`desktop/cua.json` 固定上游 CuaDriver 与 SHA-256。
- `make runtime-sync` 同步当前平台开发资源。安装包包含 `resources/agent-browser`、`resources/browser`、`resources/cua`；运行时无需下载软件。
- 独立 CLI/Docker 的 Go 构建不包含这些运行依赖，用户按技能说明安装上游程序，或用 `AIC_AGENT_BROWSER_PATH`、`AIC_BROWSER_PATH`、`AIC_CUA_DRIVER_PATH` 指定。没有技能包安装器。

macOS 按上游要求将随包 CuaDriver.app 安装到 `/Applications/CuaDriver.app`，官方 MCP 再按应用名启动它；不增加平台安装器或启动代理。CuaDriver 保留上游 app 身份，桌面权限由系统授予该 app。驱动 daemon 生命周期由上游负责；升级正在运行的驱动后，用官方 `cua-driver stop` 停止旧 daemon，再启动新 app。Pod 关闭时回收自己的 MCP 连接与子进程。

UI 和技能说明独立发布。Browser 内容版本为 1.0.7，CUA/create_skill 保持 1.0.3。Browser UI 经 RTC 转发 agent-browser 原生帧流和键鼠输入，复用上游 daemon 的同一 CDP 连接，绘制后回传 ACK。关闭 UI 不关闭浏览器；Pod 关闭时用上游 CLI 清理自己的 daemon。

验证命令：

```sh
go test ./...
go test -race ./libs/mcpx ./libs/host
make build backend-bin
# 上游依赖已安装或由 AIC_* 环境变量指向本地 bundle：
AIC_MCP_TEST=1 go test ./libs/host -run TestUpstreamMCPThroughExec -count=1
cd desktop && npm test
```

实机集成验证工具元数据透传与 共享浏览器的人工点击/中文输入、MCP 操作、快照、截图、ACK 限流、断开观看及 Pod 关闭清理。跨平台 Go 编译不等价于 Windows/Linux 桌面权限与驱动实机验证。
