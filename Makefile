APP_NAME   := aic-pod
BIN_DIR    := dist
MAIN_DIR   := ./desktop
DOCKER_IMAGE ?= veypi/$(APP_NAME)

GOHOSTOS   := $(shell go env GOHOSTOS)
GOHOSTARCH := $(shell go env GOHOSTARCH)
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS    := -s -w -X main.version=$(VERSION)

.PHONY: build clean all docker-build docker-push

# 默认构建当前平台
build:
	@mkdir -p $(BIN_DIR)
	cd $(MAIN_DIR) && GOWORK=off CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o "../$(BIN_DIR)/$(APP_NAME)-$(GOHOSTOS)-$(GOHOSTARCH)" .
	@echo "→ $(BIN_DIR)/$(APP_NAME)-$(GOHOSTOS)-$(GOHOSTARCH)"

# 跨平台构建
all: linux-amd64 linux-arm64 darwin-amd64 darwin-arm64 windows-amd64

linux-amd64:
	@mkdir -p $(BIN_DIR)
	cd $(MAIN_DIR) && GOWORK=off GOOS=linux GOARCH=amd64 GOWORK=off CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o "../$(BIN_DIR)/$(APP_NAME)-linux-amd64" .
	@echo "→ $(BIN_DIR)/$(APP_NAME)-linux-amd64"

linux-arm64:
	@mkdir -p $(BIN_DIR)
	cd $(MAIN_DIR) && GOWORK=off GOOS=linux GOARCH=arm64 GOWORK=off CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o "../$(BIN_DIR)/$(APP_NAME)-linux-arm64" .
	@echo "→ $(BIN_DIR)/$(APP_NAME)-linux-arm64"

darwin-amd64:
	@mkdir -p $(BIN_DIR)
	cd $(MAIN_DIR) && GOWORK=off GOOS=darwin GOARCH=amd64 GOWORK=off CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o "../$(BIN_DIR)/$(APP_NAME)-darwin-amd64" .
	@echo "→ $(BIN_DIR)/$(APP_NAME)-darwin-amd64"

darwin-arm64:
	@mkdir -p $(BIN_DIR)
	cd $(MAIN_DIR) && GOWORK=off GOOS=darwin GOARCH=arm64 GOWORK=off CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o "../$(BIN_DIR)/$(APP_NAME)-darwin-arm64" .
	@echo "→ $(BIN_DIR)/$(APP_NAME)-darwin-arm64"

windows-amd64:
	@mkdir -p $(BIN_DIR)
	cd $(MAIN_DIR) && GOWORK=off GOOS=windows GOARCH=amd64 GOWORK=off CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o "../$(BIN_DIR)/$(APP_NAME)-windows-amd64.exe" .
	@echo "→ $(BIN_DIR)/$(APP_NAME)-windows-amd64.exe"

clean:
	rm -rf $(BIN_DIR)
	@echo "cleaned $(BIN_DIR)"

# Docker 镜像构建（需先编译对应平台二进制）
docker-build: linux-amd64
	docker build --platform linux/amd64 \
		-t $(DOCKER_IMAGE):$(VERSION) \
		-t $(DOCKER_IMAGE):latest \
		.
	@echo "→ $(DOCKER_IMAGE):$(VERSION) + latest"

docker-build-arm64: linux-arm64
	docker build --platform linux/arm64 \
		-t $(DOCKER_IMAGE):$(VERSION)-arm64 \
		.
	@echo "→ $(DOCKER_IMAGE):$(VERSION)-arm64"

docker-push:
	docker push $(DOCKER_IMAGE):$(VERSION)
	docker push $(DOCKER_IMAGE):latest
	@echo "→ pushed $(DOCKER_IMAGE):$(VERSION) + latest"

help:
	@echo "Usage:"
	@echo "  make                  build for current platform ($(GOHOSTOS)/$(GOHOSTARCH))"
	@echo "  make all              cross-compile all platforms"
	@echo "  make linux-amd64      build linux/amd64"
	@echo "  make linux-arm64      build linux/arm64"
	@echo "  make darwin-amd64     build darwin/amd64"
	@echo "  make darwin-arm64     build darwin/arm64"
	@echo "  make windows-amd64    build windows/amd64"
	@echo "  make docker-build     build docker image ($(DOCKER_IMAGE):$(VERSION))"
	@echo "  make docker-build-arm64 build linux/arm64 docker image"
	@echo "  make docker-push      push docker image"
	@echo "  make clean            remove $(BIN_DIR)/"
