# aic-pod 运行时镜像：Go + Node + agent-browser + Chromium 全环境。
#
# 基础镜像用 node:22-slim（debian bookworm）而非 alpine：
#   - agent-browser 的 Chromium 与系统库均为 glibc/debian 生态，
#     alpine（musl）依赖兼容性差
#   - node:22-slim 为 multi-arch，配合 TARGETARCH 支持 amd64/arm64
#
# 浏览器后端：统一用系统 chromium（apt 安装）——
#   Chrome for Testing 无 Linux ARM64 构建（上游限制），且两架构统一
#   配置更简单：ENV AGENT_BROWSER_EXECUTABLE_PATH=/usr/bin/chromium
#
# 构建：
#   make docker-build          # linux/amd64
#   make docker-build-arm64    # linux/arm64
FROM node:22-slim
ARG TARGETARCH=amd64
ARG GO_VERSION=1.26.2
# Go 下载源：默认 golang.google.cn（Google 官方中国镜像，国内可达）；
# 海外构建可覆盖为 https://go.dev/dl
ARG GO_DL_BASE=https://golang.google.cn/dl
# npm 源：默认国内镜像（npmmirror）；海外构建可覆盖为 https://registry.npmjs.org
ARG NPM_REGISTRY=https://registry.npmmirror.com
# 构建代理（docker config.json 或 --build-arg 注入，预定义 build-arg，RUN 内直接可用）：
#   docker build --build-arg HTTP_PROXY=http://127.0.0.1:7897 --build-arg HTTPS_PROXY=http://127.0.0.1:7897 .
# 注意：apt 只认小写 http_proxy/https_proxy——大写 HTTP_PROXY 对 apt 无效，
# 必须在小写导出后再跑 apt；apt.conf 用完即删，代理不固化进运行镜像。
ARG HTTP_PROXY=
ARG HTTPS_PROXY=
# apt 镜像源主机（默认清华，国内可达；海外构建覆盖为 deb.debian.org）
ARG APT_MIRROR_HOST=mirrors.tuna.tsinghua.edu.cn
# 基础工具 + chromium + Chrome 运行依赖（playwright 官方 debian 依赖清单子集）
RUN export http_proxy=${HTTP_PROXY} https_proxy=${HTTPS_PROXY:-${HTTP_PROXY}} \
  && { [ -z "${HTTP_PROXY}" ] || printf 'Acquire::http::Proxy "%s";\nAcquire::https::Proxy "%s";\n' "${HTTP_PROXY}" "${HTTPS_PROXY:-${HTTP_PROXY}}" > /etc/apt/apt.conf.d/30proxy; } \
  && sed -i "s|deb.debian.org/debian|${APT_MIRROR_HOST}/debian|g; s|security.debian.org/debian-security|${APT_MIRROR_HOST}/debian-security|g" /etc/apt/sources.list.d/debian.sources \
  && apt-get update && apt-get install -y --no-install-recommends \
  ca-certificates tzdata curl wget \
  git openssh-client \
  python3 python3-pip \
  vim grep sed gawk findutils ripgrep \
  coreutils procps net-tools \
  jq make gcc \
  chromium \
  # Chromium 运行依赖
  libnss3 libatk1.0-0 libatk-bridge2.0-0 libcups2 \
  libdrm2 libxkbcommon0 libxcomposite1 libxdamage1 libxfixes3 \
  libxrandr2 libgbm1 libasound2 libpango-1.0-0 libcairo2 \
  libglib2.0-0 fonts-liberation \
  && rm -f /etc/apt/apt.conf.d/30proxy \
  && rm -rf /var/lib/apt/lists/*
# Go 官方 tarball（版本与 go.work 一致，可 ARG 覆盖；下载源 GO_DL_BASE 可换）
RUN case "${TARGETARCH}" in \
  amd64) GOARCH=amd64 ;; \
  arm64) GOARCH=arm64 ;; \
  *) echo "unsupported TARGETARCH: ${TARGETARCH}"; exit 1 ;; \
  esac \
  && curl -fsSL "${GO_DL_BASE}/go${GO_VERSION}.linux-${GOARCH}.tar.gz" \
  | tar -C /usr/local -xz \
  && ln -s /usr/local/go/bin/go /usr/local/bin/go
# agent-browser（npm 全局安装，统一用系统 chromium 作为浏览器后端）
RUN export http_proxy=${HTTP_PROXY} https_proxy=${HTTPS_PROXY:-${HTTP_PROXY}} \
  && npm install -g agent-browser --registry=${NPM_REGISTRY}
# 浏览器后端：系统 chromium（两架构统一；无需 agent-browser install）
ENV AGENT_BROWSER_EXECUTABLE_PATH=/usr/bin/chromium
# aic 二进制（Makefile 先编译到 dist/）
COPY dist/aic-cli-linux-${TARGETARCH} /usr/local/bin/aic
RUN mkdir -p /workspace
ENV PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
  SHELL=/bin/bash \
  HOST=https://ivec.ai \
  NO_SANDBOX=true \
  WORK_DIR=/workspace \
  EXEC_TIMEOUT=30m
ENTRYPOINT ["aic"]
