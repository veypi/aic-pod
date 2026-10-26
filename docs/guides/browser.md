# 设备侧 Browser 与 CUA 接入

- 文档状态：现行实现（2026-10-23 整理）
- 说明：设备侧怎么把官方 browser/CUA 跑起来、怎么把画面转给平台、怎么回收。
  跨端画面契约见 [aic/docs/reference/remote-screen.md](../../../aic/docs/reference/remote-screen.md)；
  执行与工具服务总契约见 [hosts-tools.md](../reference/hosts-tools.md)，服务注册机制见 [host-vtools.md](../reference/host-vtools.md)；
  构建与分发见 [release.md](release.md)。

## 1. 进程模型

浏览器与 CUA 都不是自研实现：Pod 启动上游官方的 MCP 程序，工具与 schema 原样暴露。

| 服务 | 上游 | 形态 |
| --- | --- | --- |
| `browser` | `agent-browser mcp --tools core,tabs,mobile,state`（0.38.2） | 内置服务，注册为 vsh 根命令 `browser` |
| `cua` | `cua-driver mcp`（0.33.2） | 内置服务，注册为 vsh 根命令 `cua` |
| `blender` | Blender addon 通道 | `tcp://127.0.0.1:9866`，含 doctor/launch/install 生命周期动词（详见 blender 技能手册） |

内置项的声明来自 aic-skills 对应包的 `cli/manifest.json`（pod 经 embed 读取，不硬编码）。所有者显式配置同名 `mcp.servers` 项会**整体替换**内置项（含默认有效期与权限）；`disabled: true` 关闭内置服务。stdio 服务懒启动并复用一个 SDK session，进程由 vbox 托管；一次取消不关闭服务。

## 2. 生命周期与回收

`idle_timeout` 是每个服务条目的空闲有效期：服务启动或最后一次使用（取连接或在途请求结束）后经过该时长无任何调用即被回收，下次调用重新懒启动；在途请求算活跃；缺省空值 = 随 Pod 常驻。内置 browser/CUA 默认 30m。

内置 browser 的 MCP 连接与上游 daemon 分离——到期时显式用上游 CLI 关闭它自己的 daemon（连同受管浏览器），彻底释放资源；所有者覆盖的 browser 项自管 daemon，Pod 不代管。UI 实时画面挂载期间按分钟刷新 browser 的空闲截止时间（画面通道不经过 MCP session），避免观看中被回收。

Pod 退出时清理专用运行目录中的上游 daemon。服务或启动权限配置更新时关闭旧连接，撤销相关临时服务授权。

## 3. 画面通道

内建 Browser UI 的 `aic-browser` RTC data channel 复用同一 peer 的设备认证与 `mcp.browser` 命令权限，双向转发该 agent-browser daemon 的原生 WebSocket 消息。Pod 只做分片传输，不解析浏览器工具、不建立 CDP 连接。

- 通道注册走通用流机制：`rtc.Config.Streams` 按 DataChannel 标签现查 vtools 登记表，任何实现 `vtools.Streamer` 的服务都能获得同形态通道（见 [host-vtools.md](../reference/host-vtools.md) §4）。
- 上游连接地址从 Pod 专用运行目录的 `aic.stream` 读取（动态绑定端口），固定连回环地址。
- 帧格式：4 字节大端长度头 + 16 KiB 连续片段，单消息上限 16 MiB；UI 绘制完当前帧后才把 ACK 原样回上游，Pod 不提前确认。
- 连接请求 `pacing=ack&maxFps=15`；慢链路至多积压一帧。

## 4. 图片预览

`mcp call <server> <tool> --image-preview WIDTHxHEIGHT`（调试直通）与服务根命令 `<alias> <tool> --image-preview` 显式启用设备端图片预览，每边 1..4096。结果中的 image 等比缩入指定尺寸（小图不放大），JPEG 质量从 70 起按需降低，单图不超过 600 KiB；上游 `structuredContent`、`capture_id`、`annotations` 与其他元数据保留，图片 `_meta["aic.dev/image-preview"]` 增加 `source_width/source_height/width/height`，供调用方还原上游截图坐标。CUA UI 使用 `1280x720`。

## 5. 权限边界

调用门为 exec 规则 `mcp.<alias>`（`grant cmd mcp.<alias>` 放行该服务全部工具与流）。内置 browser/CUA 以 Pod 设备权限运行，以访问 Chrome 私有状态和系统桌面；第三方服务默认受沙箱约束。进程 cwd/env/沙箱固定；按调用 session 获得的临时文件权限不扩展常驻进程沙箱。服务授权是设备能力授权，原生 fs 规则不隔离浏览器或桌面操作。browser 默认用上游 `--filesystem-root` 限制显式文件参数为 Pod 工作目录，其余业务权限由上游服务负责。CUA 权限诊断只读：仅调用 `check_permissions({prompt:false})` 展示，不替用户修改系统权限。

## 6. 分发

Desktop 分发固定版本上游运行依赖（agent-browser runtime 随 Desktop extraResources 打包）；独立 CLI 用户按技能说明安装上游软件。内置 browser 的 MCP 前端退出不会结束上游 daemon，故 Pod 在空闲回收时显式用上游 CLI 关闭自己的 daemon。
