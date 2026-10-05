# AIC Pod 架构

现行实现：2026-10-05。调用契约见 [设备 MCP](hosts-tools.md)。

Pod 是设备执行端，负责身份验证、原生命令、文件服务、官方 browser/CUA 与第三方 MCP 连接。技能内容位于 cloud，Pod 不维护技能安装表，不托管技能 UI/API。

| 模块 | 唯一职责 |
| --- | --- |
| cli / desktop | 入口、设备设置、Go 后端生命周期 |
| cfg | 配置读取/校验/修改/原子保存、设备绑定与本机设置面（`aic config get|set`）；MCP 配置是设备所有者提供的固定启动信息 |
| libs/host | 装配身份、执行器、文件服务与 mcp command；唯一运行权限状态 permissionState（FS/net/SSH/cmd 一份编译基表 + 会话临时授权，vbox 提供匹配） |
| libs/mcpx | 官方 SDK 服务连接管理与一个 vsh mcp 命令 |
| protocol | 全部线上契约单包：tool 请求/响应与错误、FS 数据面类型、hosts_nats/hosts_rtc 签名与认证信封、RTC 票据与信令、subject/caps/凭证、路径可解析层（批次 2 合包；NATS/RTC 信封各自存在，类型 Nats*/Rtc*/FS* 前缀区分） |
| libs/execution | vsh、日志、前台等待和 bg 任务 |
| vbox | 原生进程和 OS 沙箱、取消回收 |
| libs/hostfs | 文件数据面 |
| runtime、desktop/vendor | 固定版本的上游程序与发行资源，不实现工具 |
| ../aic-skills | 静态技能内容、云端 UI/API、普通脚本资源 |

AI 使用 exec/vsh：普通 CLI 原生调用，有状态工具使用 `mcp call <server> <tool> --input JSON`。UI 经 RTC execCall 调用同一 mcp command，读取命令的 JSON stdout；没有 UI MCP 客户端。exec/fs 直接进入现有执行器和文件服务；没有平台 MCP server 或平台工具副本。文件 HTTP proxy 只转发原生 fs 请求。

本地 MCP stdio 进程按设备配置懒启动并共享，HTTP 服务用标准 Streamable HTTP 连接。服务不属于首次调用的会话，不随某次 shell 的 cwd/env 改变；单次取消只取消请求。配置或设备权限更新时重建连接；关闭 Pod 回收服务。bg 只管理普通执行任务。

权限来自可信传输身份和本地规则（cfg 授权字段；设置修改重启生效，permanent grant 即时生效——候选完整编译校验 → 原子保存 → 原子发布基表）。`mcp.<alias>` 是服务调用门，内置桌面服务以 Pod 设备权限运行；第三方进程默认使用设备沙箱。原生 fs 规则不能隔离浏览器/桌面操作；browser 显式文件参数由上游 filesystem-root 检查。临时会话 grant 不改变共享进程的启动权限。

Browser UI 使用上游页面列表、导航、快照和截图，CUA UI 使用上游权限诊断。没有自研 MCP server、工具重命名、页面/元素 ID 映射或媒体控制层。

构建、内容发布与软件安装分开，见 [构建与发行](release-architecture.md)。
