# vsh 与 MCP 执行边界

现行实现：2026-10-05。设备契约见 [hosts-tools.md](hosts-tools.md)。

- 外层 `exec.1host` 唯一选择执行端；mcp 命令不再选择目标。
- cloud/设备执行完整 vsh 脚本。内建、CLI、脚本、管道、重定向和 bg 使用现有执行器，原生程序经 vbox 启动。
- 设备 `mcp tools/describe/call/read` 只接配置的独立服务，不按技能注册命令、不生成 flags；参数使用 JSON，stdin 可传 `--input -`。
- page 接受一个字面 command，按 session 和现有窗口 ID 调用 handler；通过 commands 和 --help 发现用法。复用 vsh 语法解析器，不建立另一套 shell，也不实现 MCP。页面文件继续直接调用 PageFS。
- exec/fs、技能搜索/读取、云端 HTTP API 都没有 MCP 副本。Cloud 当前没有 MCP 服务配置或 mcp 命令。
- 服务门为 `mcp.<alias>`；共享服务的 cwd/env/沙箱来自设备配置，不随某次 shell 改变。配置更新或 Pod 退出时回收服务，单次取消不关闭整个服务。
- UI 与 AI 都按原生 request_id 取消执行，Pod 内 SDK 将上下文取消传给 MCP 服务。取消不回滚副作用；响应丢失不自动重放。
- `skill search/load` 读取云端静态内容，download 保存 ZIP，fork 创建副本。安装脚本、CLI 或 MCP 程序按说明执行。

旧技能 provider、设备安装表、包根命令、通用媒体流已删除，没有兼容入口。
