# ==============================================================================
# AIC 打包流程
#
# 产品：
#   desktop（主产品，aic-*）     : Electron 壳 + Go 后端子进程（win 托盘 / mac 菜单栏常驻）
#   cli（aic-cli-*）             : 命令行 host agent
#   browser（aic-browser.zip）   : Chrome 扩展
#   docker（veypi/aic-pod）      : 容器内运行 cli
#
# 构建产物（dist/）：
#   aic-desktop-mac-<arch>.dmg          desktop mac（electron-builder，dmg）
#   aic-desktop-win-<arch>.exe          desktop windows（NSIS）
#   aic-desktop-linux-<arch>.AppImage   desktop linux
#   aic-cli-<os>-<arch>          cli 全平台
#   aic-browser.zip              Chrome 扩展
#
# 版本：desktop 版本 = desktop/package.json（Makefile desktop-version 自动同步 git 版本），
#       cli 二进制注入 git 版本（ldflags -X cfg.Version），browser 版本 = manifest.json。
# ==============================================================================

APP_NAME    := aic
CLI_NAME    := aic-cli
BIN_DIR     := dist
MAIN_DIR    := ./cli
DESKTOP_DIR := ./desktop
DOCKER_IMAGE ?= veypi/aic-pod

GOHOSTOS   := $(shell go env GOHOSTOS)
GOHOSTARCH := $(shell go env GOHOSTARCH)
# VERSION：git 版本（tag 优先，无 tag 时为短 commit hash），注入 cli 产物与 desktop/package.json
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
# Windows 资源版本号：纯数字点分（go-winres 不接受 dirty 后缀）
WIN_VERSION := $(shell echo $(VERSION) | sed 's/^v//; s/-.*//')
LDFLAGS    := -s -w -X github.com/veypi/aic-pod/cfg.Version=$(VERSION)

BROWSER_DIR := browser
BROWSER_OUT := $(BIN_DIR)/aic-browser.zip

.PHONY: build cli-all desktop-all release clean help build-browser \
        docker-build docker-build-arm64 docker-push run-docker-test \
        cli-windows-amd64
# 注意：cli-<os>-<arch> / desktop-<os>-<arch> 等模式目标（cli-%/desktop-%/desktop-darwin-%/...）
# 不能列入 .PHONY——make 3.81 对 phony 目标只认显式规则，模式规则匹配的 phony 目标
# 会报 "Nothing to be done"（3.82 修复）。

# ==============================================================================
# cli（aic-cli-*，纯 Go 交叉编译）
# ==============================================================================

build:
	@mkdir -p $(BIN_DIR)
	cd $(MAIN_DIR) && CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o "../$(BIN_DIR)/$(CLI_NAME)-$(GOHOSTOS)-$(GOHOSTARCH)" .
	@echo "→ $(BIN_DIR)/$(CLI_NAME)-$(GOHOSTOS)-$(GOHOSTARCH)"

cli-all: cli-linux-amd64 cli-linux-arm64 cli-darwin-amd64 cli-darwin-arm64 cli-windows-amd64

# 通用跨平台模板：make cli-<os>-<arch>
cli-%:
	@mkdir -p $(BIN_DIR)
	@os=$$(echo $* | cut -d- -f1); arch=$$(echo $* | cut -d- -f2-); \
	ext=""; [ "$$os" = "windows" ] && ext=".exe"; \
	cd $(MAIN_DIR) && GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o "../$(BIN_DIR)/$(CLI_NAME)-$$os-$$arch$$ext" .
	@echo "→ $(BIN_DIR)/$(CLI_NAME)-$*"

# windows 资源（icon + version info，go-winres）
cli-windows-amd64:
	@mkdir -p $(BIN_DIR)
	@echo "→ generating Windows resources (icon + version info)..."
	cd $(MAIN_DIR) && go-winres make --in ../resources/winres.json --arch amd64 \
		--product-version $(WIN_VERSION) --file-version $(WIN_VERSION) --out rsrc
	cd $(MAIN_DIR) && GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o "../$(BIN_DIR)/$(CLI_NAME)-windows-amd64.exe" .
	@echo "→ $(BIN_DIR)/$(CLI_NAME)-windows-amd64.exe"

# ==============================================================================
# desktop（aic-* 主产品，Electron 壳 + Go 后端子进程）
#
# 架构：Electron 主进程（main.js）spawn Go 后端二进制（bin/aic-backend，即 cli
# 编译产物），经 AIC_PORT_FILE 握手后加载本地壳页面；窗口控制走 preload IPC。
# 打包：electron-builder（须在目标平台构建，无法交叉），产物 dist/aic-desktop-*。
# ==============================================================================

# Go 后端二进制：dev 运行（desktop/bin/aic-backend）与 electron-builder
# extraResources（resources/backend/）共用
backend-bin:
	@mkdir -p $(DESKTOP_DIR)/bin
	cd $(MAIN_DIR) && CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o "../$(DESKTOP_DIR)/bin/aic-backend" .
	@echo "→ $(DESKTOP_DIR)/bin/aic-backend"

# electron-builder 依赖安装（node_modules）
desktop-deps:
	cd $(DESKTOP_DIR) && npm install

# 同步 git 版本到 package.json（electron-builder 产物版本取自 package.json）
desktop-version:
	@node -e "const fs=require('fs');const p=JSON.parse(fs.readFileSync('$(DESKTOP_DIR)/package.json','utf8'));p.version='$(VERSION)'.replace(/^v/,'');fs.writeFileSync('$(DESKTOP_DIR)/package.json',JSON.stringify(p,null,2)+'\n')"

.PHONY: backend-bin desktop-deps desktop-version

desktop-all: desktop-darwin-amd64 desktop-darwin-arm64 desktop-windows-amd64

# macOS：dmg（electron-builder，arm64 runner 构建 arm64 / x64 runner 构建 x64）
desktop-darwin-%:
	$(MAKE) desktop-version backend-bin
	@arch=$$(echo $* | sed 's/amd64/x64/'); \
	cd $(DESKTOP_DIR) && npx electron-builder --mac --$$arch
	@echo "→ $(BIN_DIR)/aic-desktop-mac-$*.dmg"

# Windows：NSIS exe（需 Windows runner / wine）
desktop-windows-%:
	$(MAKE) desktop-version backend-bin
	cd $(DESKTOP_DIR) && npx electron-builder --win
	@echo "→ $(BIN_DIR)/aic-desktop-win-$*.exe"

# Linux：AppImage（需 Linux runner）
desktop-linux-%:
	$(MAKE) desktop-version backend-bin
	cd $(DESKTOP_DIR) && npx electron-builder --linux
	@echo "→ $(BIN_DIR)/aic-desktop-linux-$*.AppImage"


# ==============================================================================
# Docker（容器内运行 cli）
# ==============================================================================

docker-build: cli-linux-amd64
	docker build --platform linux/amd64 --build-arg TARGETARCH=amd64 \
		-t $(DOCKER_IMAGE):$(VERSION) \
		-t $(DOCKER_IMAGE):latest \
		.
	@echo "→ $(DOCKER_IMAGE):$(VERSION) + latest"

docker-build-arm64: cli-linux-arm64
	docker build --platform linux/arm64 --build-arg TARGETARCH=arm64 \
		-t $(DOCKER_IMAGE):$(VERSION)-arm64 \
		-t $(DOCKER_IMAGE):latest \
		.
	@echo "→ $(DOCKER_IMAGE):$(VERSION)-arm64 + latest"

docker-push:
	docker push $(DOCKER_IMAGE):$(VERSION)
	docker push $(DOCKER_IMAGE):latest
	@echo "→ pushed $(DOCKER_IMAGE):$(VERSION) + latest"

run-docker-test:
	@case $$(uname -m) in \
		x86_64) make docker-build ;; \
		arm64|aarch64) make docker-build-arm64 ;; \
	esac
	docker run --rm \
		--env-file .env \
		$(DOCKER_IMAGE):latest

# ==============================================================================
# Browser / Release / Clean
# ==============================================================================

build-browser:
	@mkdir -p $(BIN_DIR)
	rm -f $(BROWSER_OUT)
	cd $(BROWSER_DIR) && zip -r ../$(BROWSER_OUT) . \
		-x "*.DS_Store" \
		-x "dist/*" \
		-x "*.test.js"
	@echo "→ $(BROWSER_OUT)"

release: cli-all desktop-all build-browser
	gh release create $(VERSION) \
		--title "$(VERSION)" \
		--generate-notes \
		$(BIN_DIR)/*
	@echo "→ GitHub release $(VERSION) created"

clean:
	rm -rf $(BIN_DIR)
	rm -f $(MAIN_DIR)/rsrc_windows_*.syso
	@echo "cleaned $(BIN_DIR)"

help:
	@echo "Usage:"
	@echo "  make                  build cli for current platform ($(GOHOSTOS)/$(GOHOSTARCH))"
	@echo "  make cli-all          cross-compile cli (linux/darwin/windows × amd64/arm64)"
	@echo "  make desktop-all      build desktop (darwin amd64/arm64 + windows amd64)"
	@echo "  make desktop-darwin-amd64 / -arm64   macOS dmg（electron-builder，须在 mac 构建）"
	@echo "  make desktop-windows-amd64           Windows NSIS exe（须在 windows 构建）"
	@echo "  make desktop-linux-amd64             Linux AppImage（须在 linux 构建）"
	@echo "  make backend-bin      build Go 后端二进制 → desktop/bin/aic-backend（dev 运行）"
	@echo "  make docker-build     build docker image ($(DOCKER_IMAGE):$(VERSION))"
	@echo "  make docker-build-arm64 build linux/arm64 docker image"
	@echo "  make docker-push      push docker image"
	@echo "  make run-docker-test  test docker image locally (auto arch, loads .env)"
	@echo "  make release          cross-compile (cli + desktop) + Chrome Extension + GitHub release"
	@echo "  make build-browser    package Chrome Extension → dist/aic-browser.zip"
	@echo "  make clean            remove $(BIN_DIR)/"
