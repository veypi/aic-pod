# 设备直连与统一调用

2026-09-21：本文件原 hosts/1 设计已由三协议方案替代。旧 CommandService/Runtime、通用 session/operation/resource、hosts-control/data/live 通道及兼容入口均已删除。

当前设计与可调用格式：

- [统一结构图与状态归属](hosts-protocols-proposal.md)
- [三协议实现、后台执行与输出、RTC stream](hosts-tools.md)
- [前端文件 SDK](../../aic/ui/hosts/README.md)
- [HTTP 文件代理](../../aic/docs/hosts-proxy-protocol.md)

AI 使用内建 fs 和 exec。扩展只注册为 exec.commands，hosts_tools/1 维护一套声明；前端经 hosts_rtc/1，服务器经 hosts_nats/1。FS、exec、Browser、CUA 分别维护自己的业务状态。文件代理保持 fs-only；双向异步 stream 仅 RTC 前端可用。
