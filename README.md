# aic-pod

AIC Pod 客户端 — 部署在 PC/服务器上，通过 NATS WebSocket 连接 AIC 平台，提供命令执行和文件操作能力。

## 配置参数

| 环境变量 | CLI 参数 | 默认值 | 必填 | 说明 |
|---|---|---|---|---|
| `ENV_KEY` | `-key` | | ✅ | 环境凭证，格式 `<id>.<ver>.<secret>.<uid>` |
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
  -e ENV_KEY="<env_id>.<cred_ver>.<secret>.<uid>" \
  veypi/aic-pod:latest

# 完整参数
docker run -d \
  --name aic-pod \
  --restart unless-stopped \
  -e ENV_KEY="<env_id>.<cred_ver>.<secret>.<uid>" \
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
aic-pod -key "<env_id>.<cred_ver>.<secret>.<uid>"

# 完整参数
aic-pod \
  -key "<env_id>.<cred_ver>.<secret>.<uid>" \
  -name "dev-macbook" \
  -dir /Users/veypi/workspace \
  -exec-timeout 5m \
  -url wss://ivec.ai/aic/api/nc

# 查看版本
aic-pod -version
```

### .env 文件

在二进制同级目录创建 `.env`：

```env
ENV_KEY=<env_id>.<cred_ver>.<secret>.<uid>
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

## 构建

```bash
make                  # 编译当前平台
make all              # 跨平台编译 (linux/darwin/windows)
make docker-build     # 编译 + Docker 镜像
make docker-push      # 推送镜像
make clean            # 清理
```
