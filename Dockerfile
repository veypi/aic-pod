FROM alpine:latest

RUN apk --no-cache add \
    ca-certificates tzdata \
    bash curl wget \
    git openssh-client \
    python3 py3-pip \
    vim grep sed gawk findutils \
    coreutils procps net-tools \
    jq make gcc musl-dev

ARG TARGETARCH=amd64
COPY dist/aic-pod-linux-${TARGETARCH} /usr/local/bin/aic-pod

RUN mkdir -p /workspace

ENV PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    SHELL=/bin/bash \
    AIC_URL=wss://ivec.ai/aic/api/nc \
    WORK_DIR=/workspace \
    DEVICE_NAME= \
    EXEC_TIMEOUT=10m

ENTRYPOINT ["aic-pod"]
