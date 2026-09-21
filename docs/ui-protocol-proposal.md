# Browser / CUA 统一交互协议方案

> 历史设计记录：其中 browser/cua 的入口与状态归属已由 [三协议实现](hosts-tools.md) 替代，不是现行调用文档。

状态：设计记录，首版已实施。日期：2026-09-17。实际命令、边界与验证入口见 [ui-protocol.md](ui-protocol.md)。本文保留设计过程中的可选扩展，不作为运行时支持清单。

设计前提（用户已明确）：不考虑旧代码、旧命令和旧脚本兼容。`browser`、`cua` 按两套新工具设计，共用一份 UI 交互协议；旧实现仅作为底层能力和问题的参考，不约束新接口。

移除 Chrome 扩展产品，browser 直接面向 Electron/CDP，cua 面向原生应用/窗口。统一目标、定位、操作、观察、返回和错误语义。普通信息进入 `content`，`attrs` 仅承载图片等特殊通道。命令语法、返回和权限同步切换，不设置 legacy adapter、别名过渡期或双协议维护期。

## 1. 当前问题与设计依据

以下结论来自当前代码，而非旧文档中的能力声明：

| 当前实现 | 问题 | 本方案处理 |
|---|---|---|
| desktop 从 `browser/src/` 同步 core、argv、network-interceptor 到 vendor | 扩展成为主产品的代码上游；删除目录会破坏 desktop | desktop 独立实现，删除扩展和同步链 |
| browser 只有 13 个 action；输入、按键、滚动建议使用 eval | 常规交互需要拼 JS，缺少可验证的输入语义 | 补齐 fill/type/press/scroll 等原语 |
| browser 使用 `@eN`，cua 使用 driver token、pid/window 和另一套 browser ref | 相同动作需要学习多种定位方式 | 平台统一生成带快照身份的 ref，底层 token 留在 adapter |
| browser 返回文本、JSON 和多种 attrs；cua 返回摘要或 driver 原始文本 | 成功、观察结果、错误和附件没有统一组织方式 | 共用 UiResult 及文本渲染规则 |
| browser 的 worker store、cua 的 browser binding 含进程级状态 | 互斥锁可以防并发调用，不能防会话之间串用绑定 | 目标、ref、观察记录按 session 隔离 |
| cua 未知 flag 自动推断类型并覆盖底层参数 | 协议随上游变化，解析、分级和执行可能不同步 | 固定 schema，不开放任意底层参数透传 |

参考本机已安装工具的公开接口：

- unified-computer-use：先取得 app/browser/tab 对象，再在该对象上交互；支持持久会话。
- browser API：tab 是稳定操作对象；语义 locator、DOM ref、坐标操作是不同定位途径；capabilities 按需发现；fill 与 type 分开。
- cua-driver：快照绑定的 element token、精确窗口目标、后台投递与前台投递区分、操作后验证。

借鉴这些语义，自主确定接口。唯一语义源是结构化 `UiOperation`；推荐用命令式语法调用，JS 包装也调用同一操作模型。`action + argv` 是接入 AIC exec 的一种传输形式，不构成内部协议边界，也不要求复用旧解析器。

## 2. 插件端移除边界

### 2.1 产品和代码归属

保留 desktop 与 cli。`browser` 仍是浏览器自动化命令；删除的是 `browser/` 下的 Chrome MV3 扩展产品。

建议新目录：

```text
protocol/ui/                     # 指令 schema、结果 schema、跨语言固定向量
desktop/browser/
  core.mjs                       # 新 browser 操作分发与目标生命周期
  input.mjs                      # CDP 点击、文本、按键、滚动
  observe.mjs                    # 语义快照、ref、截图和坐标映射
  network.mjs                    # CDP 网络采集与请求归属
  core.test.mjs
desktop/browser-tool.mjs        # provider 通道装配
desktop/electron-adapter.mjs    # Electron/CDP 及原生内容池
libs/ui/                       # Go 侧公共参数、UiResult、目标/ref 管理契约
libs/host/cua*.go               # cua-driver adapter、会话生命周期
```

这是结构建议；不要求 Go 与 JS 共享执行代码。需要同源的是命令 schema、权限规则和行为测试向量。

### 2.2 实际迁移清单

1. 建立新 schema、解析器和 desktop browser 实现。可复用经过审查的 CDP/图像等底层代码，不整体搬运旧 core、旧 flag 解析器或插件状态机；网络采集直接使用 CDP。旧测试只作场景参考，按新协议建立验收向量。
2. 修改 `browser-tool.mjs`、`electron-adapter.mjs` 的路径；修改 `electron-builder.yml`、`check-asar.mjs`，保证源码进入安装包。
3. 删除 `sync-browser.mjs`、Makefile 的 `browser-sync` 和 npm 的相关 prestart/predist 调用；保留 cua 同步流程。
4. 删除余下扩展目录，包括 manifest、background、chrome-adapter、popup/options、扩展独有 SDK、PageFS 副本和 bundled NATS。
5. 删除 `build-browser`、CI browser job、release 的 browser 依赖和扩展产物入口。发版选取产物时排除历史遗留的 `dist/aic-browser.zip`，避免通配符把旧包重新发布。
6. 更新 README、desktop README、design、host_sandbox 中的测试说明、版本注释和 `browser-client.md`；历史 CHANGELOG 保留事实记录。
7. 联动 `aic/ui/page/hosts.html` 的扩展下载入口、引导文案及对应 i18n；更新 `aic/docs/instruction_sets_v2.md` §5.6、§5.10 及能力矩阵。
8. 移除扩展同源要求时，保留 AIC page 自己的 `page_fs.js`、`fsops.js` 和本地宿主连接能力。它们不属于待删除的扩展产品。

旧扩展 host 的历史记录和审计无需删库，也不能改标为 desktop。服务端是否拒绝旧扩展继续连接属于独立下线策略；本方案默认停止分发和维护，不隐式撤销用户凭证。

## 3. 传输与命令语法

推荐外部命令形态仍可通过 AIC exec 调用，原因是操作易发现、单步易审计，并能复用传输、会话和附件通道；这不表示保留旧 browser/cua 指令契约：

```json
{"action":"browser","argv":["click","@s12:e4","--target","t1"]}
```

```json
{"action":"cua","argv":["click","@s27:e4","--target","w1"]}
```

正文示例中的 `browser ...`、`cua ...` 是可读写法；线上仍传 argv 数组，不经过 shell，不新增转义层。文本、脚本、URL 各占一个数组元素。

解析后立即得到唯一规范操作，分级、执行和 JS 包装都使用它：

```json
{
  "domain": "browser",
  "op": "click",
  "target": "t1",
  "locator": {"ref": "@s12:e4"},
  "args": {"button": "left", "count": 1},
  "options": {"timeout_ms": 10000, "after": "snapshot", "delivery": "background"}
}
```

每个 op 有自己的参数 schema，未知字段或不适用参数明确报 `invalid_argument`。会话、请求 ID 和已授权资源由可信调用上下文注入，调用参数不能覆盖。需要改为直接结构化工具调用时，就暴露此 schema，无须另造一份行为协议。

统一语法：

```text
<browser|cua> <command> [subcommand] [positionals...] [flags...]

公共参数：
  --target <id>               当次目标；省略时使用本会话已绑定目标
  --timeout <duration>        等待/执行上限，如 500ms、5s；默认 10s
  --after none|snapshot|screenshot   变更后观察，默认 snapshot
  --format text|json          默认 text；只改变 content 的渲染方式
```

- 位置参数和 flag 类型由 schema 定义。仅支持 `--flag value`，不猜测布尔值、数字或未知参数；提供 `--` 终止 flag 解析。
- `--timeout` 包括排队、定位、执行和后置观察，且不得超过外层请求 deadline。wait 默认 10s，open/navigate 默认 30s；run 默认 60s、上限 300s。
- `--after` 只适用于动作，不给纯读取追加隐式观察。`snapshot` 默认纯文本，显式 `--image` 才附图片；`screenshot` 默认附图片。
- 顶层 help 只展示核心命令、定位方式与最短工作流；`help <command>` 返回完整 schema，`capabilities` 返回当前 adapter 实际能力。
- 新协议识别为 `ui/1`，与 NATS/caps 的版本分开。help 和 `capabilities` 明确公布唯一支持版本及真实支持集；不检测旧 argv 猜测版本，不要求客户端每次重复传版本。

## 4. 目标、快照与定位

### 4.1 Target

统一使用平台返回的 opaque target ID：browser 的 target 是 tab，cua 的 target 是精确窗口，显式桌面目标则是指定 display。示例 `t1`、`w1` 只是便于阅读，客户端不得推导或自造 ID。

```text
browser target list
browser target use t1
cua target list --app "Blender"
cua target use w1
```

- `target list` 不启动应用、不新建标签、不切换当前目标。
- `target use` 仅绑定本会话操作对象，不激活系统窗口、不抢焦点；可返回该目标的初始 snapshot，失败时返回绑定成功与观察失败两个事实。
- `--target` 仅影响本次调用，不改默认绑定。未绑定且无参数时返回 `target_required`；不静默操作用户前台页。
- target 表按 `(host, session_id, runtime_epoch)` 隔离。ID 在有效期内稳定，关闭或 runtime 重启后失效；旧 ID 不复用。
- 底层映射保存 tab/webContents ID 或 app/pid/window ID 及存活身份，校验 PID/窗口 ID 被复用的情况。
- session 隔离的是绑定和引用；多个会话显式选择同一真实窗口时，仍共享该窗口状态，需要资源锁与失效通知。

### 4.2 Snapshot 与 ref

```text
browser snapshot --target t1
cua snapshot --target w1 --image
```

共同返回 `target`、`snapshot`、标题、视口/窗口边界、可见文本和带 ref 的可操作元素。

```text
@s12:e1 textbox "搜索" value=""
@s12:e2 button "搜索"
@s12:e3 checkbox "记住我" checked=false
```

- ref 同时绑定 session、runtime、target、snapshot 和底层节点，展示为 `@s12:e1`。不同目标/不同会话不可混用。
- **不接受裸 `@e1` 自动套到最新快照。** 新快照中的 e1 可能已是另一个控件。
- browser 映射到 frame/document + DOM 节点身份；cua 映射到 driver element token。保留原始 role/value 信息，公共 role 只作归一化视图。
- 新 snapshot、导航、目标关闭、driver 重启或已知影响目标的变更会使旧引用失效。动作成功后保守地让旧 ref 失效，`--after snapshot` 返回新的可用引用。
- 不能声称能捕获任意外部 UI 变化。执行前仍须验证节点存活、目标身份及操作前提；底层报 stale 时返回 `stale_ref`，不换一个“看起来相似”的节点继续点击。

### 4.3 Locator

共同优先级：快照 ref → 明确的语义定位 → 已观察图像的坐标。

```text
browser click @s12:e2
cua click @s27:e2
browser click --role button --name "登录"
cua click --role button --name "保存"
browser fill --label "邮箱" --text "name@example.com"
browser click --css "form button[type=submit]"
cua click --at 320 180 --snapshot s27
```

- 一个动作只能选择一种 locator；语义条件内部可组合 role/name。ref、css、at 之间互斥。
- role/name、label 默认精确匹配；需要包含匹配显式用 `--contains`。命中多个返回 `ambiguous_target`，不会默认选第一个。
- browser 专属 CSS、frame、test-id 等定位能力按 capabilities 暴露。cua 不伪装支持 DOM/CSS。
- 坐标必须关联有图片的 snapshot。坐标以**实际投递图片的像素空间**计量，adapter 根据缩放、裁剪、DPR 和窗口原点映射到执行空间；映射记录保存在 snapshot 中。
- browser 默认是 viewport 图像，cua 默认窗口图像。全页截图用于阅读，不能直接当 viewport 点击坐标；需要先滚动并获取 viewport snapshot。
- 图像被压缩或服务端再次缩放时，坐标映射也必须同步。若端到端无法确定模型收到的尺寸，拒绝坐标动作并要求获取可定位截图，不能猜缩放比例。
- `press/type` 省略 locator 时，只操作目标内已观察且仍可确认的焦点；无法确认返回 `focus_required`。

## 5. 核心命令表

共同命令保持相同拼写和语义；adapter 未实现的操作返回 `unsupported`，并由 capabilities 提前告知。

| 命令 | 语义 | 示例 |
|---|---|---|
| `capabilities` | 当前目标或 backend 支持的命令、定位与投递能力 | `cua capabilities --target w1` |
| `target list/use/current` | 发现、绑定、读取当前目标 | `browser target use t1` |
| `snapshot` | 结构、文本、ref；`--image` 可带图 | `cua snapshot --image --query "保存"` |
| `screenshot` | 获取图像，创建可用于坐标的 snapshot | `browser screenshot` |
| `read [locator]` | 提取目标或元素的可读文本 | `browser read @s12:e3` |
| `get <field> [locator]` | 读取 title/text/value/checked/bounds 等字段 | `cua get value @s27:e1` |
| `click [locator]` | 点击；`--button left/right/middle`、`--count 1/2` | `cua click @s27:e2 --count 2` |
| `fill [locator] --text <text>` | 将可编辑内容替换为完整文本 | `browser fill @s12:e1 --text "张三"` |
| `type [locator] --text <text>` | 在选区/光标处插入文本，不预先清空 | `cua type --text "新增内容"` |
| `press [locator] --key <key>` | 单键或组合键 | `browser press --key "ControlOrMeta+Enter"` |
| `scroll [locator] --dx N --dy N` | 滚动目标或指定容器 | `cua scroll --dy 480` |
| `move [locator]` | 移动虚拟指针/hover | `browser move @s12:e2` |
| `drag --from <ref> --to <ref>` | 在同一目标内拖动 | `cua drag --from @s27:e2 --to @s27:e5` |
| `set [locator] --value <JSON>` | 设置非文本控件的目标状态 | `browser set @s12:e3 --value true` |
| `wait <condition>` | 等待明确条件 | `browser wait --text "保存成功" --timeout 5s` |

补充语义：

- `fill/type` 承诺文本结果；`press` 承诺键盘动作。fill 对目标不可编辑或不能可靠替换的情况报 `unsupported`，不偷偷改成向未知焦点输入。
- browser 常规点击/输入优先采用受控 CDP/浏览器输入路径；直接 `el.click()`、改 `.value` 或合成 DOM Event 的路径不能冒充 trusted input。必要时返回 `delivery` 说明真实投递方式。
- cua 的 AX 设置、输入法和剪贴板实现细节留在 adapter；涉及系统剪贴板或输入法状态变化需在结果 warnings 中说明，并遵守现有投递等级约束。
- `press` 使用 `Enter/Tab/Escape/ArrowDown` 等标准键名和 `Control/Meta/Alt/Shift` 修饰键；`ControlOrMeta` 按宿主系统展开。统一键名字典，不另设 key/hotkey/keypress 同义入口。
- `scroll` 的 dx/dy 单位为逻辑像素，正值右/下。driver 只能按刻度投递时返回 `unit_approximation`；不能保证像素距离的 adapter 不宣称精确滚动。数值描述输入请求，滚动后的距离由观察验证。
- drag 坐标形为 `--from-at X Y --to-at X Y --snapshot S`；跨窗口拖动留作扩展，初版不隐式混合两个坐标空间。
- `set` 的 JSON 标量用于 checkbox/switch/slider，字符串或字符串数组用于 select；按控件类型验证并可回读。slider 超界报错，不静默截断。
- wait 的条件为 `--text`、`--ref R --state visible|hidden|enabled|disabled`、`--role/--name`、`--value`、browser 专属 `--url/--load`。失效 ref 直接报 stale；等待新页面元素优先用语义 locator。
- `wait --ms N` 表达纯延时；不建议自动加入固定 sleep。`snapshot --query` 过滤展示，不改变 ref 的目标或含义；截断明确标出并给出完整产物路径。

## 6. 专属能力

### Browser

```text
browser open <url>                   新建 tab、绑定并返回初始观察
browser navigate <url>               当前 target 导航
browser back | forward | reload
browser close --target t1            关闭指定 tab；不自动再创建
browser network list|get|clear ...
browser console list|clear ...
browser dialog accept|dismiss ...
browser upload <ref> --file <path>    路径仍走 host 文件授权
browser download <ref>               先装监听再触发动作，返回实际下载产物
browser eval --code <js>             页面 JS 逃生口，执行上下文明确为 page
```

`open` 只负责新建目标，`navigate` 只负责当前 target 导航，关闭后不自动重建。支持 http/https 和创建空白页的 `about:blank`。目标只用稳定 ID 寻址，列表序号不具有身份语义。

下载需将 tab、触发请求和 session 目录在开始前绑定，不能等下载事件到来时再读取进程级 currentSessionDir。不把打开另一 tab 的下载记入当前调用。超时后的下载状态可通过返回的 transfer ID 查询；上传/下载不得绕过现有文件授权。

### CUA

```text
cua apps                            应用发现（非窗口目标）
cua open --app "Blender"             启动应用；唯一窗口可绑定，多窗口则列候选
cua menu --path '["File","Save"]'   JSON 路径避免菜单名称包含 > 的歧义
cua window bounds --x X --y Y --width W --height H
cua activate --target w1             显式前台激活
cua clipboard read|write ...
cua cursor on|off|state|motion|theme ...
cua doctor
```

cua 的 `open` 接受 `--app`；不强求应用启动与 URL 导航使用同样的资源参数。共同约定是返回目标和初始状态。

职责从第一版就确定：页面内容自动化全部归 browser，cua 负责原生窗口、菜单及浏览器外壳。不设置 cua 的第二套页面操作指令。browser 的可选 driver backend 使用相同操作模型：

```text
browser connect --backend driver --isolated
browser connect --backend driver --app "Google Chrome" --window <native-window-id>
browser target list
browser target use t2
browser snapshot
browser click @s31:e4
browser disconnect
```

首期 desktop browser 只接 Electron；driver browser 可后续实现，未提供时不声明该能力并返回 unsupported。接入用户现有 profile 属于额外资源授权；不能自动开启调试或复制登录状态。不因为旧 cua 曾支持某个子命令，就为它保留入口。

## 7. 输出协议：UiResult → content

内部统一为一个 UiResult。默认输出简洁文本，`--format json` 输出同一结果的 JSON；这两种格式都是 `content` 字符串，不把业务对象塞进 attrs。

建议内部字段：

```text
protocol: "ui/1"
domain: "browser" | "cua"
op: string
state: completed | waiting | rejected | error
target?: {id, kind, title, url?, app?, bounds?}
action?: {performed: true | false | "unknown", delivery?}
observation?: {snapshot, text, elements?, image?, truncated, observed_at}
data?: object                 # get、列表、脚本返回等 action-specific 内容
artifacts?: [{kind, path, mime?, bytes?}]
warnings?: [{code, message}]
error?: {code, message, retryable, recovery?}
```

所有指令采用同样的外观，按存在性省略块，不输出几十个空字段：

```text
[ui/1] browser.snapshot state=completed
target: t1 kind=tab title="账户设置"
snapshot: s12

[observation]
@s12:e1 textbox "姓名" value=""
@s12:e2 button "保存"

[artifacts]
text: /absolute/session/.ui/s12.txt
```

动作默认附后置观察，降低一次动作再一次 snapshot 的往返：

```text
[ui/1] cua.click state=completed
target: w1 kind=window title="设置"
action: performed=true delivery=accessibility
snapshot: s28

[observation]
text "已保存"
@s28:e1 button "完成"
```

统一错误：

```text
[ui/1] browser.click state=error
target: t1
action: performed=false

[error]
code: stale_ref
message: "@s12:e2 已失效"
recovery: "browser snapshot --target t1"
```

协议细节：

- text 是模型可读投影，字符串按 JSON 字符串转义，网页/应用内容放在 observation/data 块，不把其中的文本解释为协议控制信息。程序应读取 `--format json`，不要从自然语言正文恢复对象。
- 外层 `ToolResponse.state/error/need_approval` 保留；外层 state 是权威状态。内部 state 从外层统一生成，避免两套状态相互矛盾。
- browser/cua 已识别请求的校验、拒绝、审批、执行结果均由统一 renderer 输出 UI 正文，服务端与 host 同期接入。未能建立请求上下文的连接/鉴权错误属于传输层，不伪装成已执行的 UI 操作。
- 成功只证明动作已投递/完成，不证明用户业务目标实现；等待“已保存”、value 回读或页面状态才是后置验证。
- 动作已成功但后置截图失败：仍返回 completed、performed=true，带 `observation_failed` warning。禁止因“没拿到截图”自动重复点击提交。
- 超时或进程断开而无法判断动作是否发生：返回 error、performed=unknown、retryable=false，先重新观察再决定后续动作。
- 正文预算建议 12 KiB：优先保留目标、结果、可操作 ref；全文保存 `.ui/`。截断后的文本和 JSON 都必须完整合法，包含 truncated 与产物路径。

### attrs 只负责特殊通道

| 字段 | 处理 |
|---|---|
| `image_data` | 保留；仍为 data URI，沿用现有图片投递链 |
| `image_path` | 保留；仅平台内部落盘/提取用途 |
| `image_compressed` | 保留；正文 observation.image 也描述实际投递尺寸 |
| `path` | 不使用；附件路径统一进入正文 artifacts |
| `action/rows/truncated/closed/tab_id/ref/...` | 新 UI 协议不再作为业务 attrs；放 UiResult/content |
| `status/error` | 不使用；由外层 state/error 与 UiResult 表达，平台消费者同步调整 |

初版一条响应最多内联一张图片；批处理的中间图落盘，只内联显式指定的最终观察图。未来多图扩展独立定义，不发明 `image_data_1` 等临时键。

## 8. 权限与执行模型

权限由**操作副作用、实际资源、投递方式**共同决定，browser/cua 的名称本身不决定等级。执行前形成不可被 adapter 降级的规范操作计划：

```text
parse → resolve target/locator → authorize → lock → revalidate → execute → observe
```

target/locator 的解析只是元数据解析；读取目标内容也必须先有观察权限，不能在 authorize 之前执行页面求值或触发 UI。加锁后复核目标、deadline 与授权仍有效。

| 操作类别 | 典型操作 | AIC 等级映射 |
|---|---|---|
| observe | 目标发现、snapshot/read/get/screenshot、纯条件 wait | Read |
| interact | 目标内 click/fill/type/press/scroll/set、open/navigate/close | Write |
| takeover | activate、foreground 投递、显式桌面输入 | Danger |
| attach-existing | 开启/接入用户现有浏览器 profile 的调试控制 | Danger |
| code | host 上的 run；可任意修改页面的 eval | Danger |

- 两个工具都按每次规范操作计算等级；命令声明使用 Read 基线，不再给整个 browser 加 Write 地板。代码执行统一列入 code，不根据脚本文本猜测是否只读。
- `target use` 只绑定并观察，属于 observe。network/console clear、clipboard write 等按实际效果归 interact，不由顶层词名判成只读。
- 资源作用域包括 host/session、具体 tab/window/profile/display 和需要访问的文件；获准控制某个窗口，不代表获准控制其他窗口或整个桌面。
- 普通 target 内交互沿用会话已授予的操作级别，不逐次打断。takeover、attach-existing、code 由平台高等级策略处理；后台失败仅返回 `delivery_unavailable`，不能自行扩权重试。
- 上传所读文件、下载所写路径继续经过文件资源授权。网络响应/控制台内容、系统剪贴板等读取能力在 capabilities 中明示所属资源；不能用 UI 命令绕过对应资源边界。
- `--delivery background|foreground` 是授权与执行共享的同一个参数。调用方不能通过底层 driver 参数别名覆盖它；unsupported 参数在进入执行前就报错。
- 命令 schema、权限分类与固定向量同源，Go 分级和 JS 执行不各自扫描任意 argv。

执行约束：

- 同一个实际 tab/window 的操作和后置观察在同一锁内；session 状态分别保存。系统剪贴板、IME、前台输入使用 host 全局锁。初期 driver 单连接全局串行可保留，不把“支持会话隔离”误写成“支持物理输入并行”。
- 在获得锁后重新校验 deadline、目标身份和 ref；driver 重启须递增 runtime_epoch，清除失效绑定。不能自动重放已可能发生副作用的动作。
- 请求重试以 `(session_id, msg_id)` 做去重：未执行的 waiting 请求允许审批后继续；已完成请求返回缓存结果；执行中不再次派发。进程重启后没有可靠记录时，明确动作状态未知，不声称 exactly-once。

## 9. 批处理与可选 JS 接口

先完成单命令统一，再让 `browser run`、`cua run` 共用其上的 JS 包装。两者注入相同 `ui` API，不维护另一套动作语义：

```javascript
await ui.target("t1");
let s = await ui.snapshot();
let r = await ui.fill(s.ref({ role: "textbox", name: "邮箱" }), "name@example.com");
r = await ui.click(r.observation.ref({ role: "button", name: "保存" }));
await ui.wait({ text: "保存成功", timeout: "5s" });
return r;
```

对象方法只将参数转成同一规范操作；普通文本字段和 SDK helper 分开，`ref()` 必须唯一匹配。每一步使用上一动作返回的新观察，不把旧 ref 带过页面变化。

- 对应调用 `browser run --code <js>` / `cua run --file <path>`；整个批处理返回同一 UiResult，data 放逐步结果和 return 值。
- run 使用独立脚本生命周期，不要求跨调用保留 JS 变量；持久的是 session target/ref/backend 生命周期。统一返回与调用 API，不复用旧 cua.* 接口。
- run 明确属于宿主代码执行，资源能力必须与实际进程隔离措施一致。`node:vm`、隐藏 require、只注入 ui 都不构成安全沙箱，不得据此降低等级；脚本内的 ui 调用仍逐步走统一授权。若暂未建立明确的宿主代码执行边界，首版不声明 run 能力。
- 中途失败返回已执行步骤和失败位置，不默认回滚或从头重跑；逐步调用也走相同目标、权限与 deadline 校验。

## 10. 一次性切换边界

发布时同时切换命令 schema、help、权限分类、host/desktop 实现、服务端响应处理与平台文档。新进程只注册 ui/1，不保留旧参数解析、旧脚本 API、裸 ref 或 tab 序号寻址。

切换前停止接收旧工具调用，等待已执行的动作结束，取消尚未派发的旧队列；再替换实现并重发能力声明。不能把旧排队请求重新解释成新语义，也不能在升级后自动重放执行状态不明的动作。UI 会话建立新的 runtime_epoch，并重新发现目标和观察状态。

可保留用户浏览器数据、登录态和审计记录；代码兼容性与用户数据保留是两件事。旧调用或脚本需要直接更新，错误提示指向新 help，不在运行时自动翻译。

## 11. 落地顺序与验收

**P0：新规范与能力实现。** 定义 UiOperation、UiResult、参数 schema 和权限矩阵；实现 desktop 自有 CDP 模块及 cua-driver adapter。旧实现可在开发分支中供对照，不进入新协议的发布产物。验收以新行为规范为准。

**P1：协议最小闭环。** 完成 schema、UiResult、target/ref 生命周期，两个 adapter 的 snapshot/click/fill/type/press/scroll；补齐 browser 原生输入。默认动作后返回文本观察，图片按需。同步 vcore help、levels、AIC 协议文档与响应消费者。

**P2：高级交互。** 条件 wait、非文本控件 set、drag、上传下载事件绑定、专属命令归一；让 run 使用相同操作模型。

**P3：可选后端扩展。** 将 cua 的 typed browser 家族迁入 browser driver backend；新后端只声明实际支持能力，不为补齐矩阵保留扩展产品。

**发布门槛：插件与旧实现一次性退场。** 新核心闭环可用后，删除插件、旧 core/解析器/runner 接口和同步链，更新构建、下载引导、caps/help/文档及平台消费者。新鲜 checkout 无扩展目录也能启动和打包，产物中不存在 legacy adapter；未完成的可选功能不声明能力。P2/P3 可后续发布，不要求为等待高级功能保留旧协议。

必要验收场景：

1. 同一工作流只替换 browser/cua、target 和观察所得 ref，即可完成填写、点击、等待及回读。
2. 中文、引号、换行、空字符串在 fill/type 中准确保留；fill 替换、type 插入、press 按键行为不同且明确。
3. 多个语义匹配、目标关闭/ID 复用、旧快照 ref、跨会话 ref、driver 重启均明确失败，不误点。
4. 两个会话交错选目标不会串绑定；共用真实目标会使另一会话观察过期；异步下载归属正确。
5. 缩图、HiDPI、裁剪、滚动后坐标有正确映射；全页图和未知缩放不会被拿来直接点击。
6. 动作成功而观察失败不重试副作用；deadline/重试/审批重发不会重复执行已完成动作。
7. image_data 仍能到达模型，普通信息无需读取 attrs；text/json 来自同一个 UiResult，截断不破坏结构。
8. help、capabilities、schema、权限分类、真实支持集一致；未实现参数不被静默忽略或透传覆盖。

## 12. 代码与参考索引

当前实现：

- [公共 schema](../protocol/ui/schema.json)、[Go 协议](../protocol/ui/protocol.go)
- [browser core](../desktop/browser/core.mjs)、[CDP 输入/观察](../desktop/browser/cdp.mjs)
- [desktop 通道](../desktop/browser-tool.mjs)、[Electron adapter](../desktop/electron-adapter.mjs)
- [CUA adapter](../libs/host/cua_ui.go)、[驱动传输](../libs/host/cua.go)
- [权限分级](../libs/vcore/levels.go)、[请求去重和输出](../libs/host/ui_response.go)
- [平台指令集](../../aic/docs/instruction_sets_v2.md)、[平台 host 引导](../../aic/ui/page/hosts.html)

本机参考接口（仅用于本提案设计依据，不成为项目运行依赖）：

- `/Users/veypi/.codex/plugins/cache/openai-bundled/unified-computer-use/26.901.51231/resources/browser-description.md`
- `/Users/veypi/.codex/plugins/cache/openai-bundled/unified-computer-use/26.901.51231/resources/computer-description.md`
- `/Users/veypi/.codex/plugins/cache/openai-bundled/browser/26.901.51231/docs/api.json`
- `/Users/veypi/.agents/skills/cua-driver/SKILL.md`
