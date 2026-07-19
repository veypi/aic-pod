# ==============================================================================
# 发版流程
# ==============================================================================
#
# 1. 修改版本号  desktop/main.go 中 var version = "x.y.z"
#
# 2. 查看上次发版至今的变更:
#      git log $(git describe --tags --abbrev=0 2>/dev/null || echo HEAD)..HEAD --oneline
#
# 3. 提交代码:
#      git add -A && git commit -m "chore: bump version to vx.y.z"
#
# 4. 打 tag 并推送:
#      git tag -a vx.y.z -m "vx.y.z"
#      git push origin main
#      git push origin vx.y.z
#
# 5. 编译所有平台 + 创建 GitHub Release:
#      make release
#
# 6. (可选) 构建并推送 Docker 镜像:
#      make docker-build && make docker-push
#
# ==============================================================================

APP_NAME   := aic-pod
BIN_DIR    := dist
MAIN_DIR   := ./desktop
DOCKER_IMAGE ?= veypi/$(APP_NAME)

GOHOSTOS   := $(shell go env GOHOSTOS)
GOHOSTARCH := $(shell go env GOHOSTARCH)
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS    := -s -w -X main.version=$(VERSION)

BROWSER_DIR := browser
BROWSER_OUT := dist/aic-browser.zip

.PHONY: build clean all docker-build docker-push release run-docker-test build-browser

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
	docker build --platform linux/amd64 --build-arg TARGETARCH=amd64 \
		-t $(DOCKER_IMAGE):$(VERSION) \
		-t $(DOCKER_IMAGE):latest \
		.
	@echo "→ $(DOCKER_IMAGE):$(VERSION) + latest"

docker-build-arm64: linux-arm64
	docker build --platform linux/arm64 --build-arg TARGETARCH=arm64 \
		-t $(DOCKER_IMAGE):$(VERSION)-arm64 \
		-t $(DOCKER_IMAGE):latest \
		.
	@echo "→ $(DOCKER_IMAGE):$(VERSION)-arm64 + latest"

docker-push:
	docker push $(DOCKER_IMAGE):$(VERSION)
	docker push $(DOCKER_IMAGE):latest
	@echo "→ pushed $(DOCKER_IMAGE):$(VERSION) + latest"

# 本地 Docker 测试（自动检测架构并构建，加载 .env）
run-docker-test:
	@case $$(uname -m) in \
		x86_64) make docker-build ;; \
		arm64|aarch64) make docker-build-arm64 ;; \
	esac
	docker run --rm \
		--env-file .env \
		$(DOCKER_IMAGE):latest

# 跨平台编译 + Chrome Extension + GitHub Release
release: all build-browser
	gh release create $(VERSION) \
		--title "$(VERSION)" \
		--generate-notes \
		$(BIN_DIR)/*
	@echo "→ GitHub release $(VERSION) created"

# Chrome Extension 打包
build-browser:
	@mkdir -p $(BIN_DIR)
	cd $(BROWSER_DIR) && zip -r ../$(BROWSER_OUT) . \
		-x "*.DS_Store" \
		-x "dist/*"
	@echo "→ $(BROWSER_OUT)"

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
	@echo "  make run-docker-test  test docker image locally (auto arch, loads .env)"
	@echo "  make release          cross-compile + create GitHub release"
	@echo "  make build-browser    package Chrome Extension → dist/aic-browser.zip"
	@echo "  make clean            remove $(BIN_DIR)/"
