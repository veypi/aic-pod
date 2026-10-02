#!/bin/sh
# 构建 cua skill 包产物（与 browser/hello 包同约定：仓库为源，产物不入库）：
# cli/bin/ 两个 provider 二进制（安装 = 构建后 skill install 本目录；
# skillpackages embed 在 pod 构建期收进——先于 go build 执行）。
set -e
cd "$(dirname "$0")"
mkdir -p cli/bin
go build -o cli/bin/cua ./provider/process
go build -o cli/bin/cua-service ./provider/service
echo "→ cli/bin/cua, cli/bin/cua-service"
