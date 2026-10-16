# host-vtools：通用工具服务注册机制（2026-10-16 定稿）

> 地位同 hosts-vsh-redesign.md：本文件是「服务注册进 vsh」的契约源。
> 动机：MCP 只是一种协议。任何协议（MCP / TCP / …）的工具适配成
> `vtools.Provider` 后直接注册为 vsh 根命令；`mcp` 命令退化为纯管理面
>（add/remove/start/stop/list + 调试直通），不再是工具调用的中转层。

## 1. 接口体系（libs/vtools）

```go
type Tool struct { Name, Description string; InputSchema json.RawMessage }

type Provider interface {                    // 一切协议适配器的最小面
    Tools(ctx context.Context) ([]Tool, error)
    Call(ctx context.Context, tool string, args map[string]any) (any, error)
}

type CallError struct{ Result any }          // 工具级失败（MCP isError）：结果照渲染，退出码 1

type ImagePreviewer interface {              // 可选：--image-preview（MCP 适配器实现）
    PreviewImages(result any, size string) (any, error)
}

type Streamer interface {                    // 可选：实时流（见 §4）
    StreamChannel() string                   // RTC DataChannel 标签，如 "aic-browser"
    Stream(ctx, caller, input <-chan []byte, send func([]byte) error) error
}
```

- `vtools.MCP(resolve)`：MCP 适配器，薄包装 `mcpx.Manager` 的共享 session
 （懒启动/空闲回收/授权中间件都在 mcpx，行为与 mcp call 一致；resolve 每次
 调用现取连接，管理器重建对已注册命令透明）。`PreviewImages` 复用
 `mcpx.PreviewToolImages`。
- `vtools.NewTCP(host:port)`：vtools/tcp v1 行分隔 JSON 协议样例
 （`{"id":N,"method":"tools"|"call",...}`），证明接口与 MCP 解耦。

## 2. 注册进 vsh

- 每个服务 alias 经 `RegisterGuarded` 注册一个根命令；**撞名即拒**（不覆盖
 内建/平台命令），缺席摘除用新增的 `Registry.Unregister`（在途调用持有
 Command 引用自然跑完；实现 `io.Closer` 的 Provider 顺带关闭）。
- 命令形态：`<alias> <tool> [--flag value]... [--input JSON|-] [--json]
 [--image-preview WxH]`；无参 = 工具目录；`<alias> <tool> --help` 按
 inputSchema 渲染参数表。
- 参数映射：inputSchema 的标量属性（string/number/integer/boolean）自动映射
 为同名 flag（布尔可裸写）；复杂对象/数组走 `--input`（JSON 对象，flag 覆盖
 同名键）；未知 flag 报错并列出可映射项；未知工具名列出可用面。
- **注册即时、连接懒惰**：登记不 connect，首次执行才连接。
- 调用门在命令边界：exec 规则 `mcp.<alias>`（与历史 `mcp call` 的门同一条
 规则，`grant cmd mcp.<alias>` 放行该服务全部工具与流）。
- `commands` 的 Discoverable 动态纳入已登记 alias。

## 3. mcp 命令 = 管理面

```
mcp list [--json]                       配置态 + 运行态（manager.Status + 登记表）
mcp add <alias> [--cwd D][--env K=V]... [--idle DUR] [--persist] <command> [args...]
mcp add <alias> --url <http(s)-url> [--persist]
mcp add <alias> --tcp <host:port> [--persist]     （落盘形态 url = tcp://host:port）
mcp remove <alias> [--persist]
mcp start|stop|restart <alias>          start = 预连接；stop = 断连不摘命令
mcp tools|describe|call|read ...        调试直通（原 mcpx.Command 原样保留）
```

- `add/remove` 引入新能力/改配置：exec 规则门 `mcp`（126 + `grant cmd mcp`
 引导，与 skill 命令同形）；`start/stop/restart` 要求 `mcp.<alias>` 已授权；
 `list` 只读放行。
- `--persist` 写 config.yaml `mcp.servers`（cfg.LoadFile→改→cfg.Save，原子写
 0600）；不带 = 进程级，重启失效。内置 browser/cua 不可 remove（禁用走配置
 `disabled: true`）。`no_sandbox` 只能所有者手改配置。
- 配置文件里 `url = tcp://host:port` 的条目由 vtools 接管（不进 mcpx.Manager）；
 `mcpx.Config.Validate` 接受 tcp scheme。

## 4. 流通道（RTC）

- `rtc.Config.Browser BrowserRelay` 已泛化为 `Streams func(label) (StreamRelay,
 bool)`：DataChannel 开启时现查 vtools 登记表（`Registry.StreamHandler`），
 运行期 add 的流式服务**无需重启 RTC**。`aic-tools` 之外的标签全部走通用
 `streamChannel`（原 browserChannel 泛化）：帧格式 uint32 大端长度 + 有序
 16 KiB 分块；单消息 ≤16 MiB；入方向必须 string ≤64 KiB，拥塞即断；
 同一 peer 每个标签至多一条。
- 权限：通道开启时过一道 `mcp.<alias>` 门；逐消息复查由 Streamer 实现方
 负责（browser 的 streamBrowser 保持逐消息复查）。挂流期间的 idle 保活
（Touch）同理由实现方负责。
- 前端 `BrowserStream`（ui/hosts/browser_stream.js）标签参数化
（`options.channel`，默认 `aic-browser`），重组/背压/15s ready 语义不变。

## 5. 内置服务 = 技能包的 cli/ 维度（2026-10-16 起）

技能包即插件单元（aic docs/skill.md）：`cli/manifest.json` 声明设备服务
（协议 kind / 别名 / 程序解析 programs / stream 通道；blender 另有私有扩展
verbs 声明生命周期动词，非通用维度），程序源码随包入库。pod 启动时经 aicskills embed 读内嵌集的清单注册服务
（`libs/host/packs.go`）：mcp-stdio 进连接管理器，tcp 由注册层直接接管；
所有者 `mcp.servers.<alias>` 同名配置整体替换内置项。

- browser/cua 的默认项是 `aic-skills/{browser,cua}/cli/manifest.json` 的数据
 （不再是 Go 硬编码）；程序解析按 path_env → bundle → 默认名（darwin 可回退
  /Applications 布局）。
- blender（`kind: tcp`）带生命周期动词：`blender doctor/launch/install`
 （自定义命令分发后透传 `Registry.RunTool`），分级报错
  blender_not_installed → blender_not_running → addon_not_connected 是产品
  功能。addon 源码 = `aic-skills/blender/cli/aic_blender_bridge.py`。
- 用户第三方包的设备安装链路（拉取 + 审批 + 注册）是 v2——manifest 格式与
  注册机制已按容纳它设计。

## 6. 明确不做（v1 边界）

- 流式端点的通用配置声明（端口文件/env/固定 URL）等第二个流式服务再抽象
 （v1 只有 browser 一个 Streamer 实例：转发机制在 pod，清单只声明 channel）。
- `aic-tools` 不加增量响应（请求-响应 + 日志补全已够用）。
- Pod 永不解析流内容（消息边界保持的纯转发）。
- `aic`（cloud）侧不变：机制仅落在物理 host（libs/host）。
