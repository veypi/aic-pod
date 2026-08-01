# ==============================================================================
# 发版流程
# ==============================================================================
#
# 1. 修改浏览器扩展版本号  browser/manifest.json 中 "version": "x.y.z"
#    （desktop 版本号由第 4 步的 git tag 决定，无需改代码）
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
# 版本号来源（各产品）
# ==============================================================================
#
#   desktop 二进制   : git 版本（git describe --tags，即 git tag vx.y.z），
#                      构建时经 ldflags -X main.version=$(VERSION) 注入；
#                      desktop/main.go 中的 var version 仅为 go run 开发兜底。
#                      docker 镜像内嵌的就是 desktop 二进制，镜像 tag 与二进
#                      制版本同源于 git 版本（非 desktop/main.go 的默认值）。
#   browser 扩展     : browser/manifest.json 的 version 字段，运行时由
#                      background.js 经 chrome.runtime.getManifest() 读取，
#                      加 "v" 前缀上报服务端（版本门禁要求 va.b.c 格式）。
#
# 两产品共享同一个 git 仓库与 tag，发版时保持 manifest.json 与 git tag 的
# 数字部分一致（manifest 0.2.0 ↔ tag v0.2.0）。
#
# ==============================================================================

APP_NAME   := aic-pod
BIN_DIR    := dist
MAIN_DIR   := ./desktop
DOCKER_IMAGE ?= veypi/$(APP_NAME)

GOHOSTOS   := $(shell go env GOHOSTOS)
GOHOSTARCH := $(shell go env GOHOSTARCH)
# VERSION：git 版本（tag 优先，无 tag 时为短 commit hash，如 v0.2.0-3-g9be711f），
# 注入 desktop 二进制（ldflags -X main.version）并用作 docker 镜像 tag。
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

# Chrome Extension 打包（先删旧 zip 防追加残留；测试文件不打包）
build-browser:
	@mkdir -p $(BIN_DIR)
	rm -f $(BROWSER_OUT)
	cd $(BROWSER_DIR) && zip -r ../$(BROWSER_OUT) . \
		-x "*.DS_Store" \
		-x "dist/*" \
		-x "*.test.js"
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
