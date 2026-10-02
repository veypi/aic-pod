# scripts/ — 包内资源脚本

随包分发的辅助脚本（构建期打进包目录，发布/安装后只读）。运行期由 cli
provider 或 ui 经包目录相对路径引用，例如 process provider 里：

    DIR="$(cd "$(dirname "$0")" && pwd)"
    sh "$DIR/../../scripts/init.sh"

大二进制不进包：用 cli/artifacts.lock.json 声明（来源/摘要/平台），安装阶段
设备侧下载校验。
