# aic-pod

设备端 fs、exec、browser/cua 与文件代理已统一采用 [hosts_tools/1 三协议架构](docs/hosts-tools.md)：Go 自管 Chrome，Desktop 仅提供默认可执行文件路径。

Browser 在后台运行真实 Chrome，统一实际版本的 UA、Client Hints、自动化标记和窗口尺寸，并持续复用独立 profile；具体行为见 [Browser 运行环境](docs/hosts-tools.md#browser-运行环境)。

AIC Pod 客户端 — 部署在 PC/服务器上，通过 NATS WebSocket 连接 AIC 平台，把本机能力
（命令执行 / 文件操作 / 浏览器自动化 / 原生 GUI 自动化 / ssh·scp 转发）注册为 LLM 可调用的工具。

| 客户端 | 形态 | 核心能力 |
|---|---|---|
| **desktop**（主产品） | Electron 壳 + Go 后端子进程 | exec（沙箱 + 三域授权）、fs、browser（Go + Chrome）、cua（原生 GUI 自动化） |
| **cli** | 单二进制 `aic` | exec（沙箱 + 三域授权，含 browser/cua/ssh/scp）、fs |

安全模型见 [docs/host_sandbox.md](docs/host_sandbox.md)，架构见 [docs/design.md](docs/design.md)，
版本变更见 [CHANGELOG.md](CHANGELOG.md)。

## 配置

CLI 与 Desktop 共享同一份配置文件：`os.UserConfigDir()/aic/config.yaml`
（macOS: `~/Library/Application Support/aic/config.yaml`），任一端的修改
（编辑文件 / 页面绑定）另一端启动即生效。

解析由 vigo/flags 承担：`AutoRegister` 声明字段，`ConfigFile` 声明文件，`Parse` 统一合并，
优先级：**显式 flag > 环境变量 > 配置文件 > 结构体默认**。

配置读取允许容错：未知字段忽略，错误字段使用默认值并保留其他有效字段；
文件无法读取或 YAML 整体损坏时使用默认配置启动，仍可进入本地设置页修复。
读取时不覆盖原文件，只有用户保存配置时才写回。
设置页通过 `flags.LoadCfg` 读取持久配置，不混入命令行或环境变量覆盖；
aic-pod 只负责设备参数的业务校验和本地 code 生成。

| 环境变量 | CLI flag | 配置键 | 默认值 | 说明 |
|---|---|---|---|---|
| `KEY` | `-key` | `key` | | 绑定凭证（必填），从 AIC 平台获取 |
| `HOST` | `-host` | `host` | `https://ivec-ai.com` | 平台地址（可带路径前缀，如 `http://127.0.0.1:4000/rses/aiv`；NATS 端点由此推断） |
| `WORK_DIR` | `-work_dir` | `work_dir` | 系统临时目录 | 命令执行工作目录 |
| `EXEC_TIMEOUT` | `-exec_timeout` | `exec_timeout` | `30m` | 后台执行超时 |
| `HOME_PATH` | `-home_path` | `home_path` | `/` | 桌面端默认打开地址（host 后的路径，如 `/`、`/a`） |
| `CODE` | `-code` | `code` | 空 | 本地管理 API 校验码（空 = 进程级随机） |
| `FS_POLICY` | `-fs_policy` | `fs_policy` | `deny` | 文件写默认立场：`deny`（仅内置根 + `fs_allow`）\| `open`（除 `fs_deny` 全放） |
| `FS_DENY` / `FS_ALLOW` | `-fs_deny` / `-fs_allow` | `fs_deny` / `fs_allow` | — | 路径 glob 拒绝 / 显式允许（允许可覆盖拒绝；裸路径覆盖子树，glob 精确匹配） |
| `NET_POLICY` | `-net_policy` | `net_policy` | `open` | 沙箱进程出站立场：`open` \| `deny`（仅 localhost） |
| `NET_DENY` / `NET_ALLOW` | `-net_deny` / `-net_allow` | `net_deny` / `net_allow` | — | 出站目标 `host:port` 拒绝 / 允许（拒绝恒优先；内建 localhost:*） |
| `SSH_POLICY` | `-ssh_policy` | `ssh_policy` | `deny` | ssh/scp 目标立场：`deny` \| `open` |
| `SSH_DENY` / `SSH_ALLOW` | `-ssh_deny` / `-ssh_allow` | `ssh_deny` / `ssh_allow` | — | ssh 目标 `host[:port]` 拒绝 / 允许（拒绝恒优先） |
| `NO_SANDBOX` | `-no_sandbox` | `no_sandbox` | `false` | 隐藏项：全局跳过 exec 沙箱（等同放弃进程级隔离，慎用；本地管理 API 不可改） |

> 三域授权（fs/net/ssh × policy/deny/allow）的完整判定式、内置根与会话级临时 grant
> 语义见 [docs/host_sandbox.md](docs/host_sandbox.md)。

本地管理 API（LocalAPI）由 api 包提供（cli/desktop 共用，vigo 框架实现）：
`aic run` 启动时在 127.0.0.1 随机端口监听并**打印带 local_code 的引导链接**，
用户浏览器访问该链接即可绑定/管理本机（与桌面端同一套通道协议）。

## 安全模型

本机能力默认受两道闸控制：

- **三域授权**（fs / net / ssh × policy / deny / allow）：每个域有默认立场（`deny`/`open`）、
  拒绝列表与显式允许列表（拒绝恒优先，显式允许可覆盖拒绝）；另有三类内置根（工作区、
  会话目录、系统临时目录）与会话级临时 grant（`grant <域> <目标>` 可加 `--permanent` 落盘）。
- **进程沙箱**（exec 调用）：darwin `sandbox-exec`（Seatbelt）/ linux bubblewrap /
  windows 受限令牌；按授予等级选 profile（read-only / workspace-write），叠加环境变量
  清洗、资源限制与网络出站闸。无可用后端时 **fail-closed**（命令不执行，绝不裸跑）。

请求级 `nosandbox` 可显式申请免沙箱：required 恒提升 Critical(4) ⇒ 必转人工审批，
且审批通过（granted 9）本身不豁免沙箱——免沙箱必须携带该标记单独审批。

## CLI

### 安装

从 [Releases](../../releases) 下载对应平台二进制，放入 `PATH` 即可。

### 用法

```bash
aic                                  # 连接运行（打印 management page 链接，浏览器访问即绑定/管理本机）
aic -key "<key>"               # 临时参数覆盖
# 查看全部参数：aic -h
```

临时参数（-host / -key / -work_dir / -exec_timeout / -home_path，或对应环境变量）只影响本次运行；
**永久生效请直接编辑 `UserConfigDir/aic/config.yaml`**（或浏览器打开 management page 绑定/设置，
页面写操作会自动持久化）。

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

Desktop 提供 ui/1 交互工具：browser 控制 Electron 工作区标签页，cua 控制本机原生窗口。
两者共用 `target / snapshot / click / fill / type / press / scroll / wait` 及结果格式。
批量操作使用 `run --code <JavaScript>` 或 `run --file <path>`，两端注入相同 `ui` API；脚本在独立受限进程运行，每一步沿用原命令权限和错误语义。
普通结果进入 content，图片经 image_data 投递；完整协议与能力边界见 [docs/ui-protocol.md](docs/ui-protocol.md)。

```text
browser open https://example.com
browser snapshot
browser click @s<snapshot>:e1
cua target list
cua target use <window-target-id>
cua snapshot --image
```

Chrome 扩展已移除；desktop browser 直接使用 CDP，不依赖插件代码或同步脚本。

## 构建

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
cli 全平台 并创建 GitHub Release；版本号只改 `cfg/config.go`（带 `v` 前缀），`desktop/package.json` 由 `make desktop-version`
从 git describe 自动同步。

依赖：Node 22+（Electron/electron-builder）、Go（后端二进制 `make backend-bin`）、`go-winres`（windows cli 资源）。
