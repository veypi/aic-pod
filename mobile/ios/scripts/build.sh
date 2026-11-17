#!/usr/bin/env bash
#
# build.sh — 构建 ivec AI iOS 壳
#
# 用法：
#   bash scripts/build.sh                # 模拟器 Debug 构建
#   bash scripts/build.sh device         # 真机 Release 归档（需签名配置）
#   bash scripts/build.sh run            # 装到 iPhone 18 Pro 模拟器并启动
#
# 注意：本机 xcode-select 可能仍指向 CommandLineTools，
#      脚本用 DEVELOPER_DIR 直连 Xcode.app，不改全局设置。
#
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

export DEVELOPER_DIR="${DEVELOPER_DIR:-/Applications/Xcode.app/Contents/Developer}"
project="IvecAI.xcodeproj"
scheme="IvecAI"
bundle="com.ivec.ai"
derived="${DERIVED_DATA:-$root/.build/DerivedData}"

if [ ! -d "$project" ]; then
  echo "工程不存在，先生成：python3 scripts/gen-project.py"
  exit 1
fi

# 工程文件是生成的：先同步一次源码清单（新增/删除 .swift 才会进 target）。
python3 scripts/gen-project.py >/dev/null

case "${1:-sim}" in
  sim)
    xcodebuild -project "$project" -scheme "$scheme" \
      -sdk iphonesimulator \
      -destination 'platform=iOS Simulator,name=iPhone 18 Pro' \
      -configuration Debug \
      -derivedDataPath "$derived" \
      build
    ;;
  device)
    xcodebuild -project "$project" -scheme "$scheme" \
      -sdk iphoneos \
      -destination 'generic/platform=iOS' \
      -configuration Release \
      -derivedDataPath "$derived" \
      -archivePath "$root/.build/IvecAI.xcarchive" \
      archive
    ;;
  run)
    xcodebuild -project "$project" -scheme "$scheme" \
      -sdk iphonesimulator \
      -destination 'platform=iOS Simulator,name=iPhone 18 Pro' \
      -configuration Debug \
      -derivedDataPath "$derived" \
      build
    app="$derived/Build/Products/Debug-iphonesimulator/IvecAI.app"
    xcrun simctl boot "iPhone 18 Pro" 2>/dev/null || true
    xcrun simctl install "iPhone 18 Pro" "$app"
    xcrun simctl launch "iPhone 18 Pro" "$bundle"
    ;;
  *)
    echo "用法: build.sh [sim|device|run]"
    exit 1
    ;;
esac
