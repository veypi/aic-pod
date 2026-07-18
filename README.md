# aic-pod

AIC Pod 客户端 — 部署在 PC/服务器上，通过 NATS WebSocket 连接 AIC 平台，提供命令执行和文件操作能力。

## 快速启动

### Docker

```bash
docker run -d \
  --name aic-pod \
  -e ENV_KEY="<env_id>.<cred_ver>.<secret>.<uid>" \
  -e DEVICE_NAME="my-pod" \
  -e WORK_DIR=/workspace \
  -v /host/workspace:/workspace \
  veypi/aic-pod:latest
```

### CLI

从 [Releases](../../releases) 下载对应平台二进制：

```bash
# macOS Apple Silicon
./aic-pod-darwin-arm64 -key "<env_id>.<cred_ver>.<secret>.<uid>"

# Linux x86_64
./aic-pod-linux-amd64 -key "<env_id>.<cred_ver>.<secret>.<uid>"

# Windows
aic-pod-windows-amd64.exe -key "<env_id>.<cred_ver>.<secret>.<uid>"
```

或使用 `.env` 文件：

```env
ENV_KEY=<env_id>.<cred_ver>.<secret>.<uid>
DEVICE_NAME=my-server
WORK_DIR=/workspace
EXEC_TIMEOUT=10m
```

```bash
./aic-pod
```

## 配置参数

| 参数 | 环境变量 | 默认值 | 说明 |
|---|---|---|---|
| `-key` | `ENV_KEY` | 必填 | 环境凭证，格式 `<id>.<ver>.<secret>.<uid>` |
| `-url` | `AIC_URL` | `wss://ivec.ai/aic/api/nc` | AIC 服务端地址 |
| `-dir` | `WORK_DIR` | `/tmp` | 命令执行工作目录 |
| `-name` | `DEVICE_NAME` | hostname | 设备展示名称 |
| `-exec-timeout` | `EXEC_TIMEOUT` | `10m` | 命令后台执行超时 |
| `-version` | | | 显示版本号 |

## 构建

```bash
make                  # 编译当前平台
make all              # 跨平台编译所有平台
make docker-build     # 编译并构建 Docker 镜像
make docker-push      # 推送镜像到 Docker Hub
make clean            # 清理编译产物
```
