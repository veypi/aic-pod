# aic-pod

AIC Pod 客户端 — 部署在 PC/服务器上，通过 NATS WebSocket 连接 AIC 平台，提供命令执行和文件操作能力。

## 配置

CLI 与 Desktop 共享同一份配置文件：`os.UserConfigDir()/aic/config.yaml`
（macOS: `~/Library/Application Support/aic/config.yaml`），任一端的修改
（编辑文件 / 页面绑定）另一端启动即生效。

解析由 vigo/flags 承担（AutoRegister 自动注册 flag + env，只需配置结构体），
优先级：**显式 flag > 环境变量 > 配置文件 > 结构体默认**。

| 环境变量 | CLI flag | 配置键 | 默认值 | 说明 |
|---|---|---|---|---|
| `KEY` | `-key` | `key` | | 绑定凭证（必填），从 AIC 平台获取 |
| `HOST` | `-host` | `host` | `https://ivec.ai` | 平台地址（可带路径前缀，如 `http://127.0.0.1:4000/rses/aiv`；NATS 端点由此推断） |
| `WORK_DIR` | `-work_dir` | `work_dir` | 系统临时目录 | 命令执行工作目录 |
| `EXEC_TIMEOUT` | `-exec_timeout` | `exec_timeout` | `30m` | 后台执行超时 |

本地管理 API（LocalAPI）由 sdk/host 提供（cli/desktop 共用，vigo 框架实现）：
`aic run` 启动时在 127.0.0.1 随机端口监听并**打印带 local_code 的引导链接**，
用户浏览器访问该链接即可绑定/管理本机（与桌面端同一套通道协议）。

## CLI

### 安装

从 [Releases](../../releases) 下载对应平台二进制，放入 `PATH` 即可。

### 用法

```bash
aic                                  # 连接运行（打印 management page 链接，浏览器访问即绑定/管理本机）
aic -key "<key>"               # 临时参数覆盖
# 查看全部参数：aic -h
```

临时参数（-host / -key / -work_dir / -exec_timeout，或对应环境变量）只影响本次运行；
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
  -e HOST=https://ivec.ai \
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

## Browser Extension

将你的 Chrome 浏览器接入 AIC 平台，AI 可以直接操作你的浏览器页面。

### 安装

**开发模式（本地加载）：**

1. 打开 `chrome://extensions/`
2. 右上角开启 **开发者模式**
3. 点击 **加载已解压的扩展程序**
4. 选择项目中的 `browser/` 目录

**从 zip 安装：**

```bash
make build-browser   # → dist/aic-browser.zip
```

然后将 `dist/aic-browser.zip` 拖入 `chrome://extensions/` 页面即可。

### 配置

安装后点击扩展图标 → **选项**，填写设置页：

| 参数 | 默认值 | 说明 |
|---|---|---|
| `key` | | 环境凭证，从 AIC 平台获取 |
| `url` | `wss://ivec.ai/aic/api/nc` | 服务端地址 |
| `deviceName` | 系统 hostname | 设备名 |
| `autoConnect` | `true` | 启动后自动连接 |
| `background` | `true` | 后台模式，新窗口不抢夺焦点 |
| `incognito` | `false` | 隐私模式，使用无痕窗口（不共享登录态） |
| `viewport` | `1280 × 720` | 默认视口 |
| `timeout` | `30s` | 操作默认超时 |

> 与 desktop 客户端不同，浏览器插件直接用你当前 Chrome 的登录状态（cookie/storage），无需额外配置 session。

### 工具能力

插件注册一个 `browser` 工具，签名与 `agent-browser` CLI 对齐：

```json
{ "action": "<action>", "argv": ["..."] }
```

| action | 说明 |
|---|---|
| `open` | 打开 URL |
| `click` / `dblclick` | 点击 / 双击元素 |
| `close` | 关闭标签页 |
| `download` | 点击触发下载 |
| `eval` | 执行 JavaScript |
| `get` | 获取页面信息 (text/html/title/url/value/attr/count/box/styles) |
| `network` | 查看网络请求 |
| `read` | 提取页面可读文本 |
| `screenshot` | 截图（支持 `--full` 全页面） |
| `snapshot` | a11y tree 快照（生成 @ref 用于元素定位） |
| `tab` | 标签页管理 (new/list/close/<N>) |
| `wait` | 等待条件 (selector/ms/--load/--text/--fn) |
| `scroll` / `hover` | 滚动 / 悬停 |
| `fill` / `press` / `select` | 表单操作 |
| `back` / `forward` / `reload` | 导航 |
| `sleep` | 暂停 |
| `cookies` | Cookie 管理 |
| `storage` | localStorage/sessionStorage 操作 |
| `pipeline` | 链式执行多个操作 |

### 对比 desktop 客户端

| 维度 | desktop (CLI/Docker) | browser (Extension) |
|---|---|---|
| 运行时 | 独立二进制 | Chrome Service Worker |
| 核心能力 | `exec` (命令执行), `fs` (文件操作) | `browser` (浏览器自动化) |
| 登录态 | 无状态 | 直接用浏览器登录态 |
| 安装 | 下载二进制 | 加载扩展 |
| 适用场景 | 服务器运维 | Web 自动化测试、网页数据采集 |

## 构建

产物：desktop 是主产品（`aic-*`），cli 是 `aic-cli-*`。

```bash
make                            # cli 当前平台 → dist/aic-cli-<os>-<arch>
make cli-all                    # cli 全平台（linux/darwin/windows × amd64/arm64）
make desktop-darwin-arm64       # desktop macOS → dist/AIC Desktop.app + aic-desktop-darwin-arm64.dmg
make desktop-darwin-amd64       # desktop macOS（Intel）
make desktop-windows-amd64      # desktop Windows exe（需先: brew install mingw-w64）
make desktop-all                # desktop 全平台（linux desktop 需容器/CI）
make build-browser              # 打包 Chrome Extension → dist/aic-browser.zip
make docker-build               # 编译 cli + Docker 镜像
make docker-push                # 推送镜像
make release                    # cli-all + desktop-all + build-browser + GitHub Release
make clean                      # 清理
```

依赖：`wails3`（desktop 资源/打包：`go install github.com/wailsapp/wails/v3/cmd/wails3@v3.0.0-beta.3`）、`go-winres`（windows 资源）、`mingw-w64`（windows desktop 交叉）。
