# 设备侧 Browser 与 CUA 接入

- 文档状态：现行实现（2026-10-31 整理）
- 说明：设备侧怎么把官方 browser/CUA 跑起来、怎么把画面转给平台、怎么回收。
  跨端画面契约见 [aic/docs/reference/remote-screen.md](../../../aic/docs/reference/remote-screen.md)；
  执行与工具服务总契约见 [hosts-tools.md](../reference/hosts-tools.md)，服务注册机制见 [host-vtools.md](../reference/host-vtools.md)；
  构建与分发见 [release.md](release.md)。

## 1. 进程模型

三者都是 aic-skills 的普通设备包：包根 `manifest.json` 通过 `host` 声明命令及其内嵌 `streams`，注册为 vsh 根命令。**browser/cua 由 pod 启动定版预装**，发行包附带预编译的 `bin/<name>`（Desktop 注入 `AIC_SKILL_BIN_DIR`）；blender 与第三方包仍 `commands install <id>`。pod 不内置任何服务逻辑。

| 服务 | 实现 | 形态 |
| --- | --- | --- |
| `browser` | 包内 Go 服务，CDP 驱动 Chrome（Launch 或 `BROWSER_CDP_URL` attach） | 启动预装，vsh 根命令 `browser`，自带实时画面 UI |
| `cua` | 包内启动器包装官方 `cua-driver mcp`（0.47.19） | 启动预装，vsh 根命令 `cua` |
| `blender` | 直接启动现成 `uvx blender-mcp` | stdio MCP，工具面完全来自上游（详见 blender 技能手册） |

owner 手工服务也走统一 `commands` 配置与命令面；stdio 服务懒启动并复用一个 SDK session，进程由 vbox 托管；一次取消不关闭服务。

## 2. 生命周期与回收

`idle_timeout` 是每个服务条目的空闲有效期：服务启动或最后一次使用（取连接或在途请求结束）后经过该时长无任何调用即被回收，下次调用重新懒启动；在途请求算活跃；缺省空值 = 随 Pod 常驻。技能包在包根 `manifest.json` 里声明（browser/CUA/blender 均 30m），回收只关闭该条目的连接与受管进程。

技能包服务的上游资源（Chrome 实例、CuaDriver daemon、Blender 进程）由各包自管，Pod 不代管上游 daemon。服务或启动权限配置更新时关闭旧连接。

## 3. 画面通道

浏览器画面入口是 browser 技能自带 UI：包内 Go 服务经 CDP screencast 推帧，流通道由包根 `manifest.json` 的 `host.<alias>.streams` 声明、command hub 落地为 `<alias>:<stream>` 标签的 RTC DataChannel；UI 与 AI 共享同一浏览器实例，人工与 AI 可同时操作。Pod 只做分片传输，不解析流内容。

- 通用流机制：`rtc.Config.Streams` 按 DataChannel 标签现查统一 command hub（见 [host-vtools.md](../reference/host-vtools.md) §4）；旧 `vtools.Streamer` 隐式接线已删除。
- 帧格式：4 字节大端长度头 + 16 KiB 连续片段，单消息上限 16 MiB；UI 绘制完当前帧后才回 ACK，Pod 不提前确认。
- 历史内置 Browser UI 的 `aic-browser` 标签（`protocol.RtcBrowserChannel`）生产零登记，仅作前端宿主 UI 默认值保留。

## 4. 图片预览

服务根命令 `<alias> <tool> --image-preview WIDTHxHEIGHT` 显式启用设备端图片预览，每边 1..4096。结果中的 image 等比缩入指定尺寸（小图不放大），JPEG 质量从 70 起按需降低，单图不超过 600 KiB；上游 `structuredContent`、`capture_id`、`annotations` 与其他元数据保留，图片 `_meta["aic.dev/image-preview"]` 增加 `source_width/source_height/width/height`，供调用方还原上游截图坐标。CUA UI 使用 `1280x720`。

## 5. 权限边界

所有统一注册命令的调用门都是别名本身（`grant cmd <alias>` 放行该服务全部工具与流）。技能包服务默认受沙箱约束，免沙箱只能由设备 owner 用 `commands config <alias> --no-sandbox` 授予；browser/CUA 通常需要免沙箱，以访问 Chrome 私有状态和系统桌面。进程 cwd/env/沙箱固定；按调用 session 获得的临时文件权限不扩展常驻进程沙箱。服务授权是设备能力授权，原生 fs 规则不隔离浏览器或桌面操作。browser 默认用上游 `--filesystem-root` 限制显式文件参数为 Pod 工作目录，其余业务权限由上游服务负责。CUA 权限诊断只读：仅调用 `check_permissions({prompt:false})` 展示，不替用户修改系统权限。

## 6. 分发

Desktop 分发固定版本上游运行依赖（如 CuaDriver 随 Desktop resources 打包）；独立 CLI 用户按技能说明安装上游软件。设备侧按各包 SKILL.md「安装与构建」从源码编译 `bin/<name>`（云端包只保存源码，不保存二进制）。
