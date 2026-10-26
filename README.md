# aic-pod

**把一台真实的机器接到模型后面。**

[![Release](https://img.shields.io/github/v/release/veypi/aic-pod?color=blue)](https://github.com/veypi/aic-pod/releases)
[![License](https://img.shields.io/badge/license-MIT-green)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.27-00ADD8)](go.mod)
[![Platform](https://img.shields.io/badge/platform-macOS%20%7C%20Windows%20%7C%20Linux-lightgrey)](#)

<!-- 演示位：录好的 30 秒 GIF 替换这一行 -->

aic-pod 是 AIC 平台的设备端客户端：它通过 NATS WebSocket 主动连出去，把本机能力
（命令执行 / 文件操作 / 浏览器自动化 / 原生 GUI 自动化 / ssh·scp 转发）注册为 LLM
可调用的工具。**本机不监听任何端口。**

它让模型在你自己的机器上**动手**——真的执行命令、真的读写文件、真的操作浏览器和原生界面。
所以这个项目相当一部分代码，花在「怎么保证它不乱来」上：

- **只有出站连接**：不接受入站连接，不需要公网 IP，不用开防火墙。
- **沙箱 fail-closed**：macOS Seatbelt / Linux bubblewrap / Windows 受限令牌；没有可用后端时**拒绝执行，绝不裸跑**。
- **统一 exec 准入 + 命令策略 + 人工审批**：所有注册命令先过 `exec_policy`，随后由命令实现落实 FS/NET/SSH 等自身策略；免沙箱请求单独提级人工审批。

**中文** | [English](README.en.md)

| 客户端 | 形态 | 核心能力 |
| --- | --- | --- |
| **desktop**（主产品） | Electron 壳 + Go 后端子进程 | exec（统一准入 + 命令策略 + 原生沙箱）、fs、browser、cua（原生 GUI 自动化） |
| **cli** | 单二进制 `aic` | exec（统一准入 + 命令策略 + 原生沙箱，含 browser/cua/ssh/scp）、fs |

设备 fs/exec 直接调用执行器和文件服务；独立服务使用 [MCP 连接](docs/reference/hosts-tools.md)。
技能是云端静态内容；browser/CUA 随 Pod 内置，默认注册并按需启动。

本文描述 2026-10-26 的现行统一命令与策略模型，核心迁移已完成；缓存、darwin 代理和
实机收尾见 [docs/development/todo.md](docs/development/todo.md)。

Browser 在后台运行真实 Chrome，统一实际版本的 UA、Client Hints、自动化标记和窗口尺寸，
并持续复用独立 profile；具体行为见 [Browser 运行环境](docs/guides/browser.md)。

## 快速开始

```bash
# 1) 在 https://ivec.ai 的设备页获取绑定凭证（KEY，仅展示一次）
# 2) 从 Releases 下载对应平台的客户端（desktop 安装包，或 CLI 单二进制）
aic -key "<你的 KEY>"      # CLI：连接平台并开始接受任务
```

```bash
# 或者用 Docker 把一台服务器变成执行节点
docker run -d --name aic-pod -e KEY="<你的 KEY>" veypi/aic-pod:latest
```

## 延伸阅读

- 文档地图：[docs/index.md](docs/index.md)
- 安全模型：[docs/reference/host-sandbox.md](docs/reference/host-sandbox.md)
- 架构：[docs/architecture/overview.md](docs/architecture/overview.md)（2026-10-26 现行统一命令模型；剩余工作见 [docs/development/todo.md](docs/development/todo.md)）
- 工具与 MCP：[docs/reference/hosts-tools.md](docs/reference/hosts-tools.md)
- Browser/CUA 接入：[docs/guides/browser.md](docs/guides/browser.md)
- 托管 SSH / git：[docs/guides/managed-ssh.md](docs/guides/managed-ssh.md)、[docs/guides/managed-git.md](docs/guides/managed-git.md)
- 构建与发行：[docs/guides/release.md](docs/guides/release.md)
- 版本变更：[CHANGELOG.md](CHANGELOG.md)

## 配置

CLI 与 Desktop 共享同一份配置文件：`os.UserConfigDir()/aic/config.yaml`
（macOS: `~/Library/Application Support/aic/config.yaml`），任一端的修改（编辑文件 /
页面绑定）另一端启动即生效。

解析由 vigo/flags 承担：`AutoRegister` 声明字段，`ConfigFile` 声明文件，`Parse` 统一合并，
优先级：**显式 flag > 环境变量 > 配置文件 > 结构体默认**。

配置读取允许容错：未知字段忽略，错误字段使用默认值并保留其他有效字段；文件无法读取或
YAML 整体损坏时使用默认配置启动，仍可用 `aic config get|set` 修复。读取时不覆盖原文件，
只有保存配置时才写回。设置面（`aic config`）与启动读取共用 `flags` 的解析/原子写，
只负责设备参数的业务校验。

| 环境变量 | CLI flag | 配置键 | 默认值 | 说明 |
|---|---|---|---|---|
| `KEY` | `-key` | `key` | | 绑定凭证（必填），从 AIC 平台获取 |
| `HOST` | `-host` | `host` | `https://ivec-ai.com` | 平台地址（可带路径前缀，如 `http://127.0.0.1:4000/rses/aiv`；NATS 端点由此推断） |
| `WORK_DIR` | `-work_dir` | `work_dir` | 系统临时目录 | 命令执行工作目录 |
| `EXEC_TIMEOUT` | `-exec_timeout` | `exec_timeout` | `30m` | 后台执行超时 |
| `HOME_PATH` | `-home_path` | `home_path` | `/` | 桌面端默认打开地址（host 后的路径，如 `/`、`/a`） |
| `EXEC_POLICY` | `-exec_policy` | `exec_policy` | `open` | 所有注册命令的统一准入立场；拒绝时命令实现不调用 |
| `FS_POLICY` | `-fs_policy` | `fs_policy` | `deny` | vbox 支撑命令的文件写默认立场：`deny`（仅内置根 + `fs_allow`）\| `open`（除 `fs_deny` 全放） |
| `FS_DENY` / `FS_ALLOW` | `-fs_deny` / `-fs_allow` | `fs_deny` / `fs_allow` | — | 路径 glob 拒绝 / 显式允许（允许可覆盖拒绝；裸路径覆盖子树，glob 精确匹配） |
| `NET_POLICY` | `-net_policy` | `net_policy` | `open` | vbox 支撑命令与 Pod `curl` 的网络立场：`open` \| `deny`；两个执行路径分别落实，不使用 `tool:` |
| `NET_DENY` / `NET_ALLOW` | `-net_deny` / `-net_allow` | `net_deny` / `net_allow` | — | 出站目标 `host:port` 拒绝 / 允许（目标按 level + 原表序匹配；内建 localhost:*） |
| `NET_UPSTREAM_PROXY` | `-net_upstream_proxy` | `net_upstream_proxy` | — | darwin deny 出站代理的显式 `http://` / `socks5://` / `socks5h://` 上游；进入 netKey，不继承宿主代理环境 |
| `SSH_POLICY` | `-ssh_policy` | `ssh_policy` | `deny` | Pod `ssh/scp/sftp` 命令的目标立场：`deny` \| `open`；不约束 `/bin/ssh` |
| `SSH_DENY` / `SSH_ALLOW` | `-ssh_deny` / `-ssh_allow` | `ssh_deny` / `ssh_allow` | — | Pod SSH 命令目标 `host[:port]` 拒绝 / 允许；匹配语义见权限文档 |
| `NO_SANDBOX` | `-no_sandbox` | `no_sandbox` | `false` | 隐藏项：全局跳过 exec 沙箱（等同放弃进程级隔离，慎用；仅配置文件/flag/env 可改） |

> 统一 exec 准入、fs/net/ssh 配置归属、内置根与会话级临时 grant 语义见
> [docs/reference/host-sandbox.md](docs/reference/host-sandbox.md)。

本机不监听任何端口（2026-09-22 去本地管理 API）：设置面 = `UserConfigDir/aic/config.yaml`，
CLI 用 `aic config get|set` / `aic bind|unbind` 读写（JSON/凭证走 stdin/stdout），
desktop 设置窗经 Electron IPC spawn 同一套子命令；保存后重启后端进程生效。

## 安全模型

本机能力按三层边界控制：

- **统一命令准入**：所有注册命令都先经过 vsh 的 `exec_policy`，拒绝时不调用命令实现。
- **命令内部策略**：vbox 支撑命令使用 FS/NET，Pod `curl` 使用 NET，Pod SSH 族使用 SSH；
  另有三类内置根（工作区、会话目录、系统临时目录）与会话级临时 grant。
- **原生进程沙箱**：vbox 支撑命令由 darwin Seatbelt / linux bubblewrap / windows 受限令牌
  落实 FS/NET，叠加环境清洗与资源限制。无可用后端或策略暂不支持时 **fail-closed**，
  对应命令不执行，不过滤规则、不改道，也不影响无关命令。

请求级 `nosandbox` 必须显式申请并经人工审批；普通审批通过本身不豁免沙箱。统一 exec
准入与命令内部策略仍然有效，不为 SSH/Git 等程序设置例外。

## CLI

### 安装

从 [Releases](https://github.com/veypi/aic-pod/releases) 下载对应平台二进制，放入 `PATH` 即可。

### 用法

```bash
aic                                  # 连接运行（config.yaml 里 key 非空则自动连接平台）
aic config get | config set          # 读写本机设置（JSON 走 stdin/stdout）
aic bind | unbind                    # 绑定/解绑平台凭证（凭证走 stdin）
aic -key "<key>"                     # 临时参数覆盖
# 查看全部参数：aic -h
```

临时参数（-host / -key / -work_dir / -exec_timeout / -home_path，或对应环境变量）只影响本次运行；
**永久生效请直接编辑 `UserConfigDir/aic/config.yaml`**（或 `aic config set` / desktop 设置窗，
保存后重启后端生效）。

### 后台运行 (macOS/Linux)

```bash
nohup aic > aic.log 2>&1 &
```

## Docker

### 构建并推送

```bash
make docker-build        # 编译 linux/amd64 并构建镜像 → veypi/aic-pod:latest
make docker-build-arm64  # 编译 linux/arm64 并构建镜像
make docker-push         # 推送到 Docker Hub
```

### 运行

```bash
# 最小启动
docker run -d --name aic-pod -e KEY="<key>" veypi/aic-pod:latest

# 完整参数
docker run -d \
  --name aic-pod \
  --restart unless-stopped \
  -e KEY="<key>" \
  -e WORK_DIR=/workspace \
  -e EXEC_TIMEOUT=30m \
  -e HOST=https://ivec-ai.com \
  -v /host/workspace:/workspace \
  veypi/aic-pod:latest
```

| Docker 参数 | 说明 |
|---|---|
| `--restart unless-stopped` | 容器退出后自动重启 |
| `-v /host:/workspace` | 将宿主机目录挂载为命令执行工作目录 |
| `-e KEY` | 必填，从 AIC 平台获取的绑定凭证 |

### 查看日志

```bash
docker logs -f aic-pod
```

## Browser / CUA

browser 与 cua 默认直接启动官方 agent-browser 0.38.2 和 cua-driver 0.33.2 的 MCP 模式，
首次调用懒启动，后续共享连接。所有工具名称、参数和结果遵循上游。Desktop 附带 Chrome 与
两套官方原生程序；独立 CLI 按技能说明安装运行依赖。

```sh
mcp tools browser
mcp call browser new_page --input '{"url":"https://example.com"}'
mcp call browser take_snapshot --input '{"pageId":1}'
mcp tools cua
```

## 构建

普通 `go build ./cli` / `go run ./cli` 包含原生执行、文件服务与 MCP manager。上游工具作为
独立进程运行，Desktop 打包时附带固定版本依赖。云端技能内容位于 aic-skills。

产物：desktop 是主产品（`aic-*`），cli 是 `aic-cli-*`。

```bash
make                            # cli 当前平台 → dist/aic-cli-<os>-<arch>
make cli-all                    # cli 全平台（linux/darwin/windows × amd64/arm64）
make desktop-darwin-arm64       # desktop macOS → dist/AIC Desktop.app + aic-desktop-darwin-arm64.dmg
make desktop-darwin-amd64       # desktop macOS（Intel）
make desktop-windows-amd64      # desktop Windows exe（需先: brew install mingw-w64）
make desktop-all                # desktop 全平台（linux desktop 需容器/CI）
make docker-build               # 编译 cli + Docker 镜像
make docker-push                # 推送镜像
make release                    # 本地全量构建 + gh release create（需各平台工具链）
make clean                      # 清理
```

desktop 打包前自动同步内置 cua-driver（`desktop/cua.json` 固定版本 + sha256 →
`vendor/cua → resources/cua`，三平台）；离线/受限网络用
`npm run cua-sync -- --asset <已下载资产>`。

**发版流程**：推送 tag `v*` 触发 CI（`.github/workflows/build.yml`）构建 desktop 全平台 +
cli 全平台 并创建 GitHub Release；版本号只改 `cfg/config.go`（带 `v` 前缀），`desktop/package.json`
由 `make desktop-version` 从 git describe 自动同步。

依赖：Node 22+（Electron/electron-builder）、Go（后端二进制 `make backend-bin`）、
`go-winres`（windows cli 资源）。

## 许可

MIT（[LICENSE](LICENSE)）。
