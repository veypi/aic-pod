# 构建与发行边界

技能内容在 **aic-skills** 维护：aic 负责云端发布与内建定版；设备侧 **browser/cua 由 pod
内嵌并在启动时定版预装**（含随发行预编译的设备程序，见 `libs/host/command_preinstall.go`），
其余设备包由 pod 用 `commands install <id>` 拉取安装。预装只是内容分发：pod 仍只解释包根
manifest 的 `host` 段，不含任何技能逻辑。Pod 不只是原生 exec/fs 与 MCP manager；它还包含
MCP 服务注册、技能包命令与流的装配。

| 产物 | 构建入口 | 包含内容 |
| --- | --- | --- |
| Pod CLI | `make build`、`make cli-<os>-<arch>` | 原生执行、FS、MCP 服务注册、技能包命令/流装配、设备传输 |
| 预装包设备程序 | `make skill-bin-<os>-<arch>` | 预编译 `browser`/`cua`/`cua_cast`（`dist/skill-bin/<os>-<arch>/` + `aic-skill-bin-<os>-<arch>.tar.gz`） |
| Desktop | `make desktop-<os>-<arch>`（含 `skill-bin-desktop`） | Go 后端、Electron、固定版本运行依赖与预装包设备程序（供技能包使用） |
| 云端技能 | aic-skills 静态 embed 与 cloud 发布流程 | SKILL.md、UI、API、表定义、包根 manifest.json 与 src/ 源码、普通资源 |

## 技能包与运行依赖

设备包形态统一：包根 `manifest.json` 通过 `host` 声明命令及其内嵌 `streams`，登记为 vsh 根命令。
**browser/cua 是官方预装包**：pod 启动时把内嵌内容定版到 `~/.aic/packages/<id>`，并把随发行预编译的
设备程序铺进包内 `bin/`（Desktop 经 `resources/skill-bin` + `AIC_SKILL_BIN_DIR`；CLI 解
`aic-skill-bin-*.tar.gz` 到可执行文件同目录），用户无需 Go 工具链；预装包不可
`commands install/update/uninstall`，随 pod 版本更新。blender 与第三方包仍走
`commands install <id>`，设备侧按包内 SKILL.md「安装与构建」准备运行程序。服务首次调用懒启动并共享
SDK session；包内可声明 `idle_timeout`（browser/cua/blender 均 30m），到期回收。包内不得声明沙箱姿态，
免沙箱只能由设备 owner 用 `commands config <alias> --no-sandbox` 授予——例外是 Windows 上的预装包：
受限令牌沙箱与 Chrome/CuaDriver 不兼容（Chrome 需 mojo/crashpad 的进程句柄权限），pod 启动定版时
为它们写入默认 `no_sandbox`（安装即视为同意随包程序在本机运行；owner 显式定过的定义不动）。

owner 手工服务同样写入统一 `commands` 配置段并由 command hub 装配；删除对应定义即停用。

blender 直接运行现成 `uvx blender-mcp`，与其他包一样经过 MCP manager；pod 不分发
自研 addon 或协议桥接。

Desktop 构建同步资源（供技能包使用，pod 自身不启动这些程序）：

- `desktop/agent-browser.json` 固定官方原生二进制及各平台 SHA-256；无 Browser 专用 Node 运行时。
- `desktop/browser.json` 固定 Chrome for Testing；`desktop/cua.json` 固定上游 CuaDriver 与 SHA-256。
- `make runtime-sync` 同步当前平台开发资源；`make skill-bin-desktop` 预编译预装包设备程序。
  安装包包含 `resources/agent-browser`、`resources/browser`、`resources/cua`、`resources/skill-bin`；运行时无需下载软件。
- 运行时注入（`desktop/main.js`）：`AIC_BROWSER_PATH` = 随包 Chrome 的**可执行文件路径**（browser
  技能包只认文件型 env，目录型提示无效），`AIC_AGENT_BROWSER_PATH` = agent-browser 可执行文件，
  `AIC_CUA_BUNDLE_DIR` = cua-driver 目录，`AIC_SKILL_BIN_DIR` = 预装包设备程序目录。
  可执行文件名的平台差异在 `desktop/runtime-paths.cjs`
  （与打包前同步/校验同一事实源）。
- 独立 CLI/Docker 不包含这些运行依赖：预装包设备程序由 `aic-skill-bin-*.tar.gz` 附带（解到 aic-cli
  同目录），其余上游程序按包内手册安装，或用 `AIC_BROWSER_PATH`、`AIC_AGENT_BROWSER_PATH`、`AIC_CUA_DRIVER_PATH` 指定。

macOS 按上游要求将随包 CuaDriver.app 安装到 `/Applications/CuaDriver.app`，官方 MCP 再按应用名启动它；不增加平台安装器或启动代理。CuaDriver 保留上游 app 身份，桌面权限由系统授予该 app。驱动 daemon 生命周期由上游负责；升级正在运行的驱动后，用官方 `cua-driver stop` 停止旧 daemon，再启动新 app。Pod 关闭时回收自己的 MCP 连接与子进程。

## 技能内容的两个版本面与两条生效链

**模块版本**（aic-skills `VERSION` / git tag，供 aic 与 aic-pod 的 Go `require` 消费）与
**技能版本**（各包 `SKILL.md` frontmatter，供内建定版指纹消费）不是一回事。当前版本不要在本文
复制，统一查 [aic-skills README 的版本表](../../../aic-skills/README.md)。

- **SKILL.md / UI / API / 文档等内容**：先 bump 对应技能版本并发布 aic-skills 模块；再由 aic
  升级 `require`、重建并重启后端。`skills.builtin` 维护事件按「包名 + 内嵌版本」指纹自动定版，
  管理员也可强制重跑；执行时整包覆盖，不做行版本大小比较。
- **包根 `manifest.json` / addon / 设备程序源码（`src/`）**：同样必须发布 aic-skills 模块；
  预装的 browser/cua 随 pod 发行更新（`make skill-bin-*` 重新预编译）；其余设备包经
  `commands install <id>` 拉取新包，并按各包 SKILL.md「安装与构建」重新编译
  `bin/<name>`。流通道由 manifest `host.<alias>.streams` 声明、command hub 落地为 `<alias>:<stream>`，
  前端必须使用同一标签。
- 本地根 `go.work` 会用兄弟目录掩盖依赖差，**不能代替发行验证**。发布前必须按下方
  `GOWORK=off` 命令验证各消费方能按已发布模块独立构建。

浏览器画面入口是 browser 技能自带 UI：包内 Go 服务经 CDP screencast 推帧，RTC 通道为
`<alias>:<stream>`（manifest `host.<alias>.streams` 声明），绘制后回传 ACK。Chrome 实例由包自管，
Pod 不代管上游 daemon。

CUA 主桌面画面由独立 `cua_cast` 服务经 `cua_cast:view` 推送 H.264 Annex B，页面用 WebCodecs
硬解；`cua` 只承担官方 MCP 权限与输入。Windows 用 DXGI `ddagrab`，macOS 用 `avfoundation`，
默认 30fps，NVIDIA 可配置 `CUA_CAST_ENCODER=h264_nvenc`。发行构建必须同时附带 `bin/cua`
与 `bin/cua_cast`；测试期 FFmpeg 仍需设备自行安装并放入 PATH。

## macOS 签名与公证

mac 产物走 **Developer ID 签名 + Apple 公证**：签名用 electron-builder 内置流程（钥匙串里
的有效 Developer ID Application 身份，CI 由 `CSC_LINK` 导入），公证用 `APPLE_API_KEY` /
`APPLE_API_KEY_ID` / `APPLE_API_ISSUER` 三项凭据（electron-builder 自动执行并 staple）。
`desktop/build/certs/` 随仓库带 G2 签发链的两张公开 CA 证书（2025 起 macOS 不预装，缺则
codesign 建不出链）。**本地无有效证书时退回 ad-hoc 深签**（用户首次打开仍需在“隐私与安全性”
手动放行），因此正式分发的包应由 CI 出。方案与风险见
[development/macos-signing.md](development/macos-signing.md)。

发布前置：GitHub Secrets 五项（`CSC_LINK` / `CSC_KEY_PASSWORD` / `APPLE_API_KEY` /
`APPLE_API_KEY_ID` / `APPLE_API_ISSUER`）已配置；`CSC_KEY_PASSWORD` 必须是 ASCII。

## 验证命令

```sh
GOWORK=off go mod tidy
GOWORK=off go build ./...
GOWORK=off go test ./...
go test -race ./libs/vtools ./libs/host
go test ./libs/rtc ./protocol
make build backend-bin skill-bin-desktop
cd desktop && npm test
```

实机集成验证工具元数据透传、共享浏览器的人工点击/中文输入、正式服务命令、快照、截图、
RTC 通道与 ACK 限流、断开观看及 Pod 关闭清理。跨平台 Go 编译不等价于 Windows/Linux 桌面权限
与驱动实机验证。
