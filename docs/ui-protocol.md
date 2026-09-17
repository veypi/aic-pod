# browser / cua：ui/1

`browser` 控制 AIC Desktop 工作区的 Electron 标签页，`cua` 控制原生应用窗口。Chrome 扩展及其构建、分发入口已移除。旧命令、旧 runner、裸 `@e1`、driver 参数透传均不兼容。

命令定义及等级的唯一来源是 [schema.json](../protocol/ui/schema.json)。Go 和 JS 各自解析同一 schema，并运行同一份固定验收用例。设计背景见 [设计记录](ui-protocol-proposal.md)，以本文和运行时 `help` / `capabilities` 为实际支持范围。

## 调用

通过 `exec` 的 `action` 选择工具，`argv` 是字符串数组，不经过 shell：

```json
{"action":"browser","argv":["fill","--label","邮箱","--text","你好@example.com"]}
```

```text
browser open https://example.com
browser wait --load complete
browser snapshot
browser fill --label "邮箱" --text "你好@example.com"
browser click --role button --name "保存"
browser wait --text "保存成功" --timeout 5s

cua target list --app TextEdit
cua target use <返回的窗口ID>
cua snapshot --image
cua click <观察返回的ref>
```

文本、URL、JSON 各占一个 argv 元素；空字符串、中文、引号和换行可原样传递。`--` 终止 flag 解析。未知命令、flag、重复 flag、混合定位方式都报错，不猜测参数类型。

- `--target ID`：本次目标；省略时使用当前会话绑定。不会改变默认绑定。
- `--timeout 500ms|5s|1m`：默认 10s，open/navigate 默认 30s，run 默认 60s，最大 5m，并受外层请求 deadline 限制。平台 browser run 的外层上限同为 5m。
- `--format text|json`：默认 text；程序解析选择 json。
- 动作支持 `--after none|snapshot|screenshot`，默认 snapshot；观察失败保留动作结果并返回 warning。
- 修改操作支持 `--delivery background|foreground`；browser 仅 background，cua foreground 要求 Danger。不能用后台失败触发自动提权。
- `help` 返回命令列表；`help "target list"` 返回单命令 schema；`capabilities` 包含后端限制。

## 目标与定位

`target list` 只发现，`target use ID` 绑定并返回观察，`target current` 读取绑定；不会激活系统窗口。未绑定返回 `target_required`。ID 是运行时生成的 opaque 字符串，不能用列表序号、PID 或旧 ID 替代。

`@s<快照身份>:eN` 同时绑定会话、目标、运行时和观察。新观察、动作或已检测到的目标变化使旧引用过期；过期返回 `stale_ref`，不自动换节点点击。会话共享真实窗口状态，但不共享绑定和 ref。browser 监听 CDP DOM/document 变化；cua 由驱动验证 element token，不能保证检测所有外部画面变化。

| 定位 | 示例 | 适用 |
|---|---|---|
| ref | `click @s012abc:e2` | 两者 |
| role/name | `click --role button --name 保存` | 两者，默认精确匹配 |
| label | `fill --label 邮箱 --text x` | 两者，使用可访问名称 |
| 包含匹配 | `click --role button --name 保存 --contains` | 两者，仍须唯一匹配 |
| CSS | `click --css "form button"` | browser |
| 图片坐标 | `click --at 120 80 --snapshot s012abc` | 两者，须为当前有图观察 |

语义命中多个返回 `ambiguous_target`。图片坐标按实际返回图片的像素计，adapter 记录缩放映射。窗口/视口几何改变后拒绝使用旧坐标；全页截图不能用于点击。截图超过 600 KiB 会压缩，正文提供压缩后尺寸，图片经平台传递时保留这些尺寸。

## 指令

| 命令 | 说明 |
|---|---|
| `snapshot [--image] [--query TEXT] [--interactive]` | 树、文本和 ref；图片按需；cua 另支持 --depth |
| `screenshot` | 树和图片；browser 可 --full（只读图） |
| `read [ref]` / `get FIELD [ref]` | text/title/url/value/checked/bounds/enabled；可用语义 locator |
| `click LOCATOR [--button left|right|middle] [--count 1|2]` | 点击 |
| `fill LOCATOR --text TEXT` | 替换文本；browser 验证 DOM 值；cua 限原生 AX 控件并回读验证 |
| `type [LOCATOR] --text TEXT` | 在选区/光标插入 |
| `press [LOCATOR] --key ControlOrMeta+A` | Enter/Tab/Escape/Backspace/Delete/方向键/Home/End/PageUp/PageDown/Space/F1..F12/字母数字 |
| `scroll [LOCATOR] --dx N --dy N` | 逻辑像素；正值向右/下；cua 按 40 像素一行近似并提示 |
| `move LOCATOR` | browser hover；cua 仅图片坐标代理光标，不产生应用 hover |
| `drag --from REF --to REF` | browser：同一视口；两者均可用 --from-at X Y --to-at X Y --snapshot S |
| `set LOCATOR --value JSON` | browser checkbox/radio/select/range；cua 由原生 AX 值能力决定 |
| `wait --text TEXT` | 等待目标可见文本包含指定值 |
| `wait LOCATOR --value JSON` / `--state visible|hidden|enabled|disabled` | 条件等待；cua 使用语义 locator，不能用旧 ref 轮询；不支持 hidden，因为 AX 无法可靠证明元素不存在 |
| `wait --ms N` | 显式毫秒延时，1..300000 |

browser `type/press` 不给定位时，只向已确认焦点元素输入。cua `type` 要求元素或图片定位；`press` 要求先观察精确窗口。cua 的 `read/get REF` 明确返回该 snapshot 的字段，不冒充实时读取；需要最新值时重新 snapshot 或用语义 locator。cua 对缺失 AX 字段返回 null，不能把 null 当作 false。

`snapshot --query` 对 name/role/value 的值做不区分大小写的包含匹配，不搜索 JSON 键名、ref 或后端元数据；browser 的附加页面文本也按行筛选。browser 滚轮输入等待事件投递及页面/嵌套容器位置在连续渲染帧中稳定，再返回观察；观察窗口内未稳定则报错，不把旧 viewport 当作已完成滚动的结果。

原生 AX 不等于 DOM。cua 不提供页面 JS、CSS 或第二套 browser 命令，AXWebArea 中的 fill/set 拒绝；页面表单使用 browser。驱动输入效果无法确认时返回 `effect_unverified`。cua 的 ref 拖动、ref move、无定位 type、full-page 图暂不支持，capabilities 明示。

### browser 专属

```text
open URL                     新建并绑定；初始观察可能仍处于加载中
navigate URL                 导航当前目标
back | forward | reload | close
wait --url URL | --load loading|interactive|complete
network list [--filter TEXT] [--limit N]
network get REQUEST_ID
network clear
console list [--filter TEXT] [--limit N]
console clear
dialog accept [--text TEXT] | dialog dismiss
upload LOCATOR --file PATH
download LOCATOR
eval --code JAVASCRIPT
```

URL 只接受 http/https/about:blank。需要加载完成时显式 wait。network/console 是 CDP 被动采集的有界历史；network get 返回已捕获元数据，不抓取 response body。eval 运行在页面上下文，等级 Danger；普通输入走 CDP trusted input，select/range 设置如需 DOM Event 会明确标注 `delivery=dom_event`。

页面在输入处理器内同步打开 alert/confirm 时，触发命令立即返回 `dialog_open`、`data.dialog` 和 `action.performed="unknown"`。原操作仍持有执行位置，普通命令返回 `browser_busy`；help/capabilities/target list/current、显式 `dialog accept/dismiss` 及 `close` 可继续调用。不会自动替调用者接受或取消弹窗，也不会重放输入。

处理弹窗后，控制命令的 `data.resumed` 给出原请求的 request_id、op、state、action 等续行结果，再返回新观察；嵌套弹窗可继续返回 `dialog_open`，按次处理。原请求的缓存结果不改写，不能用重发原输入探测完成情况。超过原请求 deadline 后，仍可处理弹窗，但续行可能报告 timeout，应重新观察实际效果。异步弹窗使用同样的显式控制。当前 Electron 的 `window.prompt()` 本身会抛出不支持异常，未产生 CDP 弹窗；`--text` 仅对后端实际报告的 prompt 有效。

upload 读取文件、download 写入会话 `.browser/` 必须经过 host 文件授权。下载先绑定 tab 和目录再点击，超时取消下载，不自动重放。未经 download 授权的自动下载被取消。新标签的下载需先显式选择目标再发起。

### cua 专属

```text
apps
open --app APP                启动应用；唯一窗口绑定，多窗口返回候选
menu --path '["File","Save"]'
window bounds --x X --y Y --width W --height H
activate                     明确前台激活，Danger
clipboard read | clipboard write --text TEXT
cursor on | cursor off | cursor state
doctor
```

原生功能取决于已安装 cua-driver 和系统辅助功能/截图授权。driver 重启或窗口进程身份改变会废弃绑定；会话结束释放绑定和驱动会话。driver 报 session ended 时，当次返回 `session_expired` 且不重放动作；下一条命令先通过 `start_session` 恢复 MCP 隐式会话，所有旧目标和 ref 失效，重新 `target list/use` 后继续。恢复失败会明确返回错误，不继续输入。没有旧参数透传，也不自动切换系统输入法。

## 批量脚本

两端都支持 `run --code JAVASCRIPT` 或 `run --file PATH`，二选一。文件路径由 host 按 workdir 解析并通过文件读取授权；内联代码和文件均最多 512 KiB。整个 run 要求 Danger(3)，每一步再次校验等级、命令、目标和文件策略。平台文件服务也必须开启，供脚本文件、截图和完整结果使用。

```json
{"action":"browser","argv":["run","--code","await ui.fill({label:'邮箱'}, '你好@example.com'); const r = await ui.click({role:'button', name:'保存'}); await ui.wait({text:'保存成功', timeout:'5s'}); return r.target;","--after","screenshot"]}
```

`browser run` 与 `cua run` 注入相同的 `ui` API，返回同样的 UiResult；API 只把参数转换成单命令 argv，不直接透传 CDP 或 cua-driver 参数。代码支持局部变量、循环、条件、async/await、try/catch、return。没有跨次脚本变量；target/ref 生命周期仍属于原会话。

```javascript
await ui.target('从 target list 获取的 ID');
const s = await ui.snapshot();
const r = await ui.fill(s.ref({role: 'textbox', name: '邮箱'}), '你好@example.com');
await ui.click(r.observation.ref({role: 'button', name: '保存'}));
return (await ui.get('value', {label: '邮箱'})).data.value;
```

`s.ref(query)` 是 `s.observation.ref(query)` 的快捷方法，只对本次观察中的 name/role/value 唯一匹配；多匹配、无匹配或缺少可操作 ref 都抛错。helper 不进入 JSON 正文。新动作产生新观察后仍须使用新 ref；不会自动修复旧引用。观察被正文预算截断时，用 `snapshot({query: ...})` 缩小范围。

| API | 参数与返回 |
|---|---|
| `ui.target(id, options)` / `ui.targets(options)` / `ui.current(options)` | 绑定、发现、读取当前目标；也可用 target.use/list/current |
| `ui.snapshot(options)` / `ui.screenshot(options)` | UiResult，观察在 observation |
| `ui.open(urlOrApp, options)` | browser URL / cua 应用名；也接受 `{app: ...}` |
| `ui.navigate(url, options)` | browser 导航 |
| `ui.click(locator, options)` / `ui.move(locator, options)` | locator 为 ref 字符串或 `{role,name}`、`{label}`、`{css}`、`{at,snapshot}` |
| `ui.fill(locator, text, options)` / `ui.set(locator, value, options)` | 参数类型与单命令相同 |
| `ui.type(text, options)` / `ui.press(key, options)` | 可在 options 中放 `locator`；例如 `ui.press('Enter', {locator: {label:'搜索'}})` |
| `ui.get(field, optionsOrRef)` / `ui.read(optionsOrRef)` | 返回 UiResult，读取值在 data |
| `ui.scroll(options)` / `ui.drag(options)` / `ui.wait(options)` | 选项按命令 schema；如 `{dy:300}`、`{locator:{label:'列表'},dy:300}` |
| `ui.upload(locator, file, options)` / `ui.download(locator, options)` | browser 文件操作，每步仍过 host 文件授权 |
| `ui.dialog.accept(options)` / `ui.dialog.dismiss(options)` | browser 显式处理弹窗 |
| `ui.command('window.bounds', options)` / `ui.call(argv)` | 完整命令入口，仍仅限当前工具域；不支持嵌套 run |
| `console.log/info/warn/error(...)` / `ui.log(...)` | 收集到 data.logs |

其他命令按名称映射：`ui.reload()`、`ui.close()`、`ui.network.list(options)`、`ui.clipboard.write({text: ...})`、`ui.window.bounds(options)` 等。多词命令使用对象方法。选项名可用 schema 原名或 camelCase（`fromAt` → `from-at`）；超时必须带单位。每个 API 都返回完整 UiResult。

必须逐步 await；并行 UI 调用返回 `concurrent_call`，脚本结束时仍未等待的调用返回 `unawaited_call`。不提供 Node、require/import、process、fetch、文件或进程对象，也没有 setTimeout；延时用 `ui.wait({ms:100})`。纯 JS Promise 没有可推进的 UI 调用时返回 `unresolved_promise`。

默认错误会抛出带 `code`、`step`（从 1 开始）、`result` 的异常并停止后续步骤。可显式捕获可恢复错误，例如：

```javascript
try {
  await ui.click({role: 'button', name: '继续'});
} catch (error) {
  if (error.code !== 'dialog_open') throw error;
  await ui.dialog.accept(); // 由脚本明确决定，不自动接受弹窗
}
return (await ui.snapshot()).observation.text;
```

`data.steps` 记录每步 index/op/duration_ms/result，`data.return` 保存 return 值，`data.steps_completed` 计成功步骤；未捕获的步骤错误带 `data.failed_step`。已执行步骤不回滚，不从头重跑。失败或超时仍保留步骤记录与动作状态；同 msg_id 重发由外层持久日志返回缓存。超出 12 KiB 的完整结果落 `.ui/` 并返回路径，内联保留完成数、失败位置和较小的 return 值。

`run --target ID` 指定脚本的初始默认目标；`ui.target/open` 更新脚本默认目标和正常的会话绑定，单步 options.target 只作用于该步。步骤复用单命令调度，整个脚本不是事务；其他请求可能在步骤之间修改同一窗口，旧 ref 仍会报 stale_ref。

默认在成功结束后观察脚本的最后默认目标，`--after none` 可关闭，`--after screenshot` 才内联最终图片。中间截图只保留产物路径，图片二进制不进入脚本或 data.steps。最终观察不计入脚本步骤数，观察失败返回 warning，不重放动作。

运行在一次性的纯 Go JS 解释器子进程内，无需安装 Node。worker 经 host OS 只读/禁网络沙箱启动；`nosandbox` 不豁免，无法落实策略返回 `script_unavailable`。当前 macOS 已实测；现有 Windows 后端缺少所需读路径/网络隔离，Linux 的 deny/glob 策略也可能被后端拒绝，不会静默降级。最多并发 4 个 worker、每次 256 步、日志 64 KiB、return 1 MiB、累计步骤结果 8 MiB。worker 设置 96 MiB Go 软内存目标，并监测超过 128 MiB 堆使用量后退出；该监测不是单次分配的硬配额。截止时间到达后终止 worker，不转后台，已投递的 UI 动作可能仍需观察或处理弹窗。

## 结果与权限

content 默认文本分块：`[ui/1]`、`[target]`、`[action]`、`[observation]`、`[data]`、`[artifacts]`、`[warnings]`、`[error]`。不存在的块省略；json 模式为同一对象：

```json
{"protocol":"ui/1","domain":"browser","op":"click","state":"completed","action":{"performed":true,"delivery":"cdp_input"},"target":{"id":"t...","kind":"tab","title":"设置"},"observation":{"snapshot":"s...","text":"...","elements":[],"truncated":false}}
```

普通信息全部在 content。attrs 只保留 image_data/image_path/image_compressed。平台追加 source 到 content，并沿用图片落盘/模型识图通道。外层 ToolResponse.state 是权威执行状态，服务端权限审批仍走平台原有控制流。

- Read(1)：观察、读取、发现、绑定、条件等待。
- Write(2)：交互、新建/关闭、剪贴板写、日志清理、上传下载；文件另授权。
- Danger(3)：run、eval、activate、foreground。
- 解析错误在权限分类处按 Danger 拒绝降级，在执行处返回明确参数错误。
- 默认正文预算 12 KiB，完整结果保存会话 `.ui/`，截断保持合法 JSON。
- 动作成功而观察失败：completed + performed=true + observation_failed。效果是否达到用户目标应通过返回观察或显式条件确认。
- 断连/超时且效果不确定：performed="unknown"，retryable=false，先观察再决定后续操作。
- host 对已授权 UI 请求按 session_id/msg_id 建立持久日志：并发重发不重复执行；完成结果重放缓存；崩溃留下 pending 时返回 outcome_unknown。同 ID 不同参数返回 request_conflict。日志保留在会话目录，不因 session_end 删除。

外部浏览器 driver backend、跨 frame 定位、跨窗口拖动不在本次实现范围，运行时不声明这些命令。

## 验证

```sh
go test -race ./protocol/ui ./libs/vcore ./libs/host
npm test --prefix desktop
cd desktop && npm run test:browser:live
```

最后一项使用隔离 Electron profile 和本机测试页，经真实 TCP provider 覆盖输入、状态设置、引用失效、截图、上传下载、网络与控制台，以及同步/嵌套/异步弹窗、权限拒绝、query 筛选和页面/嵌套滚动；还会启动 Go 集成测试，覆盖 host 脚本 worker → TCP provider → CDP 的批量输入、显式弹窗续行和最终截图。CDP prompt 参数转发由单元测试验证，Electron prompt 限制由真实页面验证。打包后 `node desktop/scripts/check-asar.mjs PATH_TO_ASAR` 递归验证模块、ui/schema.json 和 Go 后端资源，并拒绝旧 vendor/browser。

macOS 可选原生验收（仅操作新建的测试窗口，覆盖单步与批量中文输入/点击/截图，并验证主动结束本测试隐式会话后在同一 MCP 子进程恢复、旧目标失效）：

```sh
swiftc -module-cache-path /tmp/aic-ui-swiftcache -o /tmp/aic-ui-native-fixture libs/host/testdata/ui_fixture.swift
AIC_UI_NATIVE_FIXTURE=/tmp/aic-ui-native-fixture go test ./libs/host -run '^TestNativeUILive$' -count=1 -v
```
