# aic-pod

AIC Pod 客户端 — 部署在 PC/服务器上，通过 NATS WebSocket 连接 AIC 平台，提供命令执行和文件操作能力。

## 配置参数

| 环境变量 | CLI 参数 | 默认值 | 必填 | 说明 |
|---|---|---|---|---|
| `ENV_KEY` | `-key` | | ✅ | 环境凭证，从 AIC 平台获取 |
| `DEVICE_NAME` | `-name` | 系统 hostname | | 设备展示名称 |
| `WORK_DIR` | `-dir` | `/tmp` | | 命令执行工作目录 |
| `AIC_URL` | `-url` | `wss://ivec.ai/aic/api/nc` | | AIC 服务端 NATS WebSocket 地址 |
| `EXEC_TIMEOUT` | `-exec-timeout` | `10m` | | 命令后台执行最大超时（支持 30s/5m/1h 等格式） |

## Docker

### 构建并推送

```bash
make docker-build     # 编译 linux/amd64 并构建镜像 → veypi/aic-pod:latest
make docker-build-arm64  # 编译 linux/arm64 并构建镜像
make docker-push      # 推送到 Docker Hub
```

### 运行

```bash
# 最小启动
docker run -d \
  --name aic-pod \
  -e ENV_KEY="<your-env-key>" \
  veypi/aic-pod:latest

# 完整参数
docker run -d \
  --name aic-pod \
  --restart unless-stopped \
  -e ENV_KEY="<your-env-key>" \
  -e DEVICE_NAME="prod-server-01" \
  -e WORK_DIR=/workspace \
  -e EXEC_TIMEOUT=30m \
  -e AIC_URL=wss://ivec.ai/aic/api/nc \
  -v /host/workspace:/workspace \
  veypi/aic-pod:latest
```

| Docker 参数 | 说明 |
|---|---|
| `--restart unless-stopped` | 容器退出后自动重启 |
| `-v /host:/workspace` | 将宿主机目录挂载为命令执行工作目录 |
| `-e ENV_KEY` | 必填，从 AIC 平台获取的环境凭证 |

### 查看日志

```bash
docker logs -f aic-pod
```

## CLI

### 安装

从 [Releases](../../releases) 下载对应平台二进制，放入 `PATH` 即可。

### 运行

```bash
# 命令行参数
aic-pod -key "<your-env-key>"

# 完整参数
aic-pod \
  -key "<your-env-key>" \
  -name "dev-macbook" \
  -dir ~/workspace \
  -exec-timeout 5m \
  -url wss://ivec.ai/aic/api/nc

# 查看版本
aic-pod -version
```

### .env 文件

在二进制同级目录创建 `.env`：

```env
ENV_KEY=<your-env-key>
DEVICE_NAME=my-server
WORK_DIR=/workspace
EXEC_TIMEOUT=10m
AIC_URL=wss://ivec.ai/aic/api/nc
```

```bash
./aic-pod    # 自动读取 .env
```

> `.env` 和命令行参数可同时使用，命令行参数优先级更高。

### 后台运行 (macOS/Linux)

```bash
nohup aic-pod -key "..." > aic-pod.log 2>&1 &
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

插件注册一个 `web_browser` 工具，签名与 `agent-browser` CLI 对齐：

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
| 核心能力 | `exec` (命令执行), `fs` (文件操作) | `web_browser` (浏览器自动化) |
| 登录态 | 无状态 | 直接用浏览器登录态 |
| 安装 | 下载二进制 | 加载扩展 |
| 适用场景 | 服务器运维 | Web 自动化测试、网页数据采集 |

## 构建

```bash
make                  # 编译当前平台 desktop 二进制
make all              # 跨平台编译 (linux/darwin/windows)
make build-browser    # 打包 Chrome Extension → dist/aic-browser.zip
make docker-build     # 编译 + Docker 镜像
make docker-push      # 推送镜像
make clean            # 清理
```
