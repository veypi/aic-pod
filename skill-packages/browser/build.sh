#!/bin/sh
# 构建 browser skill 包产物（与 hello 包同约定：仓库为源，产物不入库）：
#   1. cli/bin/ 两个 provider 二进制（安装 = 构建后 skill install 本目录）
#   2. browser.zip 整包（desktop 打包经 extraResources 带进 resources，
#      pod 首跑 builtin 预装——AIC_BUILTIN_SKILLS）
set -e
cd "$(dirname "$0")"
mkdir -p cli/bin
go build -o cli/bin/browser ./provider/process
go build -o cli/bin/browser-service ./provider/service
echo "→ cli/bin/browser, cli/bin/browser-service"
# 整包 zip：条目相对包根（SKILL.md / cli/manifest.json / cli/bin/* / ui/**），
# 与 skillrun installZip 读侧契约一致；排除测试与平台垃圾文件（分发产物
# 不带 *.test.js / node 专用 package.json）。
rm -f browser.zip
zip -qr browser.zip SKILL.md cli ui -x '*.DS_Store' 'ui/*.test.js' 'ui/package.json'
echo "→ browser.zip"
