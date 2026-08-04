# ==============================================================================
# AIC 打包流程
#
# 产品：
#   desktop（主产品，aic-*）     : Wails v3 桌面壳（win 托盘 / mac 菜单栏常驻）
#   cli（aic-cli-*）             : 命令行 host agent
#   browser（aic-browser.zip）   : Chrome 扩展
#   docker（veypi/aic-pod）      : 容器内运行 cli
#
# 构建产物（dist/）：
#   aic-darwin-<arch>.dmg        desktop mac（.app 内嵌）
#   aic-windows-amd64.exe        desktop windows（需 mingw-w64）
#   aic-linux-<arch>             desktop linux（需容器/CI，GTK 依赖）
#   aic-cli-<os>-<arch>          cli 全平台
#   aic-browser.zip              Chrome 扩展
#
# 版本：desktop 二进制与 cli 同源 git 版本（ldflags -X main.version），
#       browser 版本 = manifest.json（发版时数字部分保持一致）。
# ==============================================================================

APP_NAME    := aic
CLI_NAME    := aic-cli
BIN_DIR     := dist
MAIN_DIR    := ./cli
DESKTOP_DIR := ./desktop
DOCKER_IMAGE ?= veypi/aic-pod

GOHOSTOS   := $(shell go env GOHOSTOS)
GOHOSTARCH := $(shell go env GOHOSTARCH)
# VERSION：git 版本（tag 优先，无 tag 时为短 commit hash），注入各产物
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
# Windows 资源版本号：纯数字点分（go-winres 不接受 dirty 后缀）
WIN_VERSION := $(shell echo $(VERSION) | sed 's/^v//; s/-.*//')
LDFLAGS    := -s -w -X main.version=$(VERSION)

BROWSER_DIR := browser
BROWSER_OUT := $(BIN_DIR)/aic-browser.zip

DESKTOP_APP := "$(BIN_DIR)/AIC Desktop.app"

.PHONY: build cli-all desktop-all release clean help build-browser \
        docker-build docker-build-arm64 docker-push run-docker-test \
        cli-windows-amd64 desktop-resources
# 注意：cli-<os>-<arch> / desktop-<os>-<arch> 等模式目标（cli-%/desktop-%/desktop-darwin-%/...）
# 不能列入 .PHONY——make 3.81 对 phony 目标只认显式规则，模式规则匹配的 phony 目标
# 会报 "Nothing to be done"（3.82 修复）。

# ==============================================================================
# cli（aic-cli-*，纯 Go 交叉编译）
# ==============================================================================

build:
	@mkdir -p $(BIN_DIR)
	cd $(MAIN_DIR) && GOWORK=off CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o "../$(BIN_DIR)/$(CLI_NAME)-$(GOHOSTOS)-$(GOHOSTARCH)" .
	@echo "→ $(BIN_DIR)/$(CLI_NAME)-$(GOHOSTOS)-$(GOHOSTARCH)"

cli-all: cli-linux-amd64 cli-linux-arm64 cli-darwin-amd64 cli-darwin-arm64 cli-windows-amd64

# 通用跨平台模板：make cli-<os>-<arch>
cli-%:
	@mkdir -p $(BIN_DIR)
	@os=$$(echo $* | cut -d- -f1); arch=$$(echo $* | cut -d- -f2-); \
	ext=""; [ "$$os" = "windows" ] && ext=".exe"; \
	cd $(MAIN_DIR) && GOWORK=off GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o "../$(BIN_DIR)/$(CLI_NAME)-$$os-$$arch$$ext" .
	@echo "→ $(BIN_DIR)/$(CLI_NAME)-$*"

# windows 资源（icon + version info，go-winres）
cli-windows-amd64:
	@mkdir -p $(BIN_DIR)
	@echo "→ generating Windows resources (icon + version info)..."
	cd $(MAIN_DIR) && go-winres make --in ../resources/winres.json --arch amd64 \
		--product-version $(WIN_VERSION) --file-version $(WIN_VERSION) --out rsrc
	cd $(MAIN_DIR) && GOWORK=off GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o "../$(BIN_DIR)/$(CLI_NAME)-windows-amd64.exe" .
	@echo "→ $(BIN_DIR)/$(CLI_NAME)-windows-amd64.exe"

# ==============================================================================
# desktop（aic-* 主产品，Wails v3）
# ==============================================================================

# 桌面资源（icns/ico，幂等）：wails3 generate icons
desktop-resources:
	cd $(DESKTOP_DIR) && wails3 generate icons -input appicon.png \
		-macfilename darwin/icons.icns \
		-windowsfilename windows/icon.ico \
		-iconcomposerinput appicon.icon \
		-macassetdir darwin

desktop-all: desktop-darwin-amd64 desktop-darwin-arm64 desktop-windows-amd64

# macOS：.app bundle（Info.plist 注入版本 + icns）+ dmg
desktop-darwin-%:
	@mkdir -p $(BIN_DIR)
	$(MAKE) desktop-resources
	@echo "→ assembling AIC Desktop.app (darwin/$*)..."
	rm -rf $(DESKTOP_APP)
	mkdir -p "$(BIN_DIR)/AIC Desktop.app/Contents/MacOS" "$(BIN_DIR)/AIC Desktop.app/Contents/Resources"
	sed 's/{{VERSION}}/$(VERSION)/g' $(DESKTOP_DIR)/darwin/Info.plist > "$(BIN_DIR)/AIC Desktop.app/Contents/Info.plist"
	cp $(DESKTOP_DIR)/darwin/icons.icns "$(BIN_DIR)/AIC Desktop.app/Contents/Resources/icons.icns"
	cd $(DESKTOP_DIR) && GOWORK=off GOOS=darwin GOARCH=$* CGO_ENABLED=1 go build -ldflags "$(LDFLAGS)" \
		-o "../$(BIN_DIR)/AIC Desktop.app/Contents/MacOS/aic-desktop" .
	@echo "→ $(BIN_DIR)/AIC Desktop.app"
	# 标准安装 dmg：.app + /Applications 链接（拖拽安装）
	rm -rf "$(BIN_DIR)/dmg-staging"
	mkdir -p "$(BIN_DIR)/dmg-staging"
	cp -R $(DESKTOP_APP) "$(BIN_DIR)/dmg-staging/"
	ln -s /Applications "$(BIN_DIR)/dmg-staging/Applications"
	hdiutil create -volname "AIC Desktop" -srcfolder "$(BIN_DIR)/dmg-staging" \
		-ov -format UDZO "$(BIN_DIR)/aic-darwin-$*.dmg" 2>&1 | tail -1
	rm -rf "$(BIN_DIR)/dmg-staging"
	@echo "→ $(BIN_DIR)/aic-darwin-$*.dmg"

# Windows：mingw-w64 交叉（brew install mingw-w64）→ exe（含图标/版本资源）
desktop-windows-%:
	@command -v x86_64-w64-mingw32-gcc >/dev/null || { echo "❌ need mingw-w64: brew install mingw-w64"; exit 1; }
	@mkdir -p $(BIN_DIR)
	$(MAKE) desktop-resources
	@echo "→ building windows desktop (icon + version resources)..."
	cd $(DESKTOP_DIR) && go-winres make --in ../resources/winres.json --arch $* \
		--product-version $(WIN_VERSION) --file-version $(WIN_VERSION) --out rsrc
	cd $(DESKTOP_DIR) && GOWORK=off GOOS=windows GOARCH=$* CGO_ENABLED=1 \
		CC=x86_64-w64-mingw32-gcc go build -ldflags "$(LDFLAGS) -H windowsgui" \
		-o "../$(BIN_DIR)/aic-windows-$*.exe" .
	@echo "→ $(BIN_DIR)/aic-windows-$*.exe"

# Linux desktop：GTK/webkit2gtk 依赖，需在 linux 环境构建（CI/容器）；产物为 AppImage。
desktop-linux-%:
	@mkdir -p $(BIN_DIR)
	$(MAKE) desktop-resources
	cd $(DESKTOP_DIR) && GOWORK=off GOOS=linux GOARCH=$* CGO_ENABLED=1 go build -ldflags "$(LDFLAGS)" \
		-o "../$(BIN_DIR)/aic-linux-$*" .
	@echo "→ $(BIN_DIR)/aic-linux-$*（打包 AppImage）"
	rm -rf /tmp/aic-appimage
	mkdir -p /tmp/aic-appimage/build
	cp "$(BIN_DIR)/aic-linux-$*" /tmp/aic-appimage/aic-desktop
	cp $(DESKTOP_DIR)/appicon.png /tmp/aic-appimage/aic-desktop.png
	cp $(DESKTOP_DIR)/linux/aic-desktop.desktop /tmp/aic-appimage/aic-desktop.desktop
	cd /tmp/aic-appimage && wails3 generate appimage \
		-binary aic-desktop -icon aic-desktop.png -desktopfile aic-desktop.desktop \
		-outputdir "$(abspath $(BIN_DIR))" -builddir /tmp/aic-appimage/build 2>&1 | tail -3
	@echo "→ $(BIN_DIR)/AIC Desktop.AppImage（linux/$*）"

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
	rm -f $(DESKTOP_DIR)/rsrc_windows_*.syso
	@echo "cleaned $(BIN_DIR)"

help:
	@echo "Usage:"
	@echo "  make                  build cli for current platform ($(GOHOSTOS)/$(GOHOSTARCH))"
	@echo "  make cli-all          cross-compile cli (linux/darwin/windows × amd64/arm64)"
	@echo "  make desktop-all      build desktop (darwin amd64/arm64 + windows amd64)"
	@echo "  make desktop-darwin-amd64 / -arm64   macOS .app + dmg"
	@echo "  make desktop-windows-amd64           Windows exe（需 mingw-w64: brew install mingw-w64）"
	@echo "  make docker-build     build docker image ($(DOCKER_IMAGE):$(VERSION))"
	@echo "  make docker-build-arm64 build linux/arm64 docker image"
	@echo "  make docker-push      push docker image"
	@echo "  make run-docker-test  test docker image locally (auto arch, loads .env)"
	@echo "  make release          cross-compile (cli + desktop) + Chrome Extension + GitHub release"
	@echo "  make build-browser    package Chrome Extension → dist/aic-browser.zip"
	@echo "  make clean            remove $(BIN_DIR)/"
