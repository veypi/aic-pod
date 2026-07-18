FROM alpine:latest

RUN apk --no-cache add ca-certificates tzdata

COPY dist/aic-pod-linux-amd64 /usr/local/bin/aic-pod

ENV AIC_URL=wss://ivec.ai/aic/api/nc \
    WORK_DIR=/workspace \
    DEVICE_NAME= \
    EXEC_TIMEOUT=10m

ENTRYPOINT ["aic-pod"]
