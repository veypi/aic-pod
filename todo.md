# TODO

## browser 归一化与设备能力三协议设计（2026-09-21 修订）

目标：不考虑历史兼容，设备能力统一为 `hosts_rtc/1`、`hosts_nats/1`、`hosts_tools/1` 三个协议。browser 作为首个落地工具，由 Go 管理独立 Chrome；Electron 只提供 Chrome 默认 executable path。browser 与 cua 已按此方向重构；当前可运行契约、已验证范围和边界见 [实现说明](docs/hosts-tools.md)。下文保留进一步优化的设计目标，不把未实现项当作现有能力。

核心决定：**AI 侧始终是内建 fs + exec。扩展通过 hosts_tool 注册为 exec 子命令，同一份声明供 AI 命令与前端 typed 调用，不增加并列 caps.tools。aic-pod 统一鉴权、指令翻译和承载；exec 内部统一一次执行、bg_* 与输出，Browser/CUA 自管常驻服务及业务对象。NATS 仅 call，stream 仅供 RTC 前端。** 本次已删除 caps.tools 和旧分发分支，详见 [统一设计](docs/hosts-protocols-proposal.md)。

### 1. 三个协议与职责

通用契约、工具声明、授权及状态归属见 [设备能力三协议设计](docs/hosts-protocols-proposal.md)，下文主要约束 browser 的业务实现。

| 协议 | 作用 |
|---|---|
| `hosts_rtc/1` | 前端经 RTC 调用设备能力；定义握手、连接和帧承载 |
| `hosts_nats/1` | 服务器经 NATS 调用设备能力；定义路由、验签、防重放和消息承载 |
| `hosts_tools/1` | 内建能力与 exec 扩展的本地接口契约，声明方法、参数/结果、权限与 call/stream，不新增 AI 工具类别 |

```text
前端 ─ hosts_rtc/1 ─ RTC adapter ───┐
                                  ├─→ aic-pod 共用分发器
服务器 ─ hosts_nats/1 ─ NATS adapter ┘   鉴权 / 翻译 / call（两通道）；stream 仅 RTC 字节转发
                                                ↓ hosts_tools/1
                                   内建 fs / exec 命令分发
                                              ↓
                             hosts_tool 一次注册为 exec 子命令
                                    browser / cua / 后续命令

browser：方法实现 → Chrome/CDP
cua：    方法实现 → 原生 GUI 驱动
```

- `hosts_tool` 是代码接口名称，`hosts_tools/1` 是协议标识。工具声明方法名、参数/返回类型、权限需求、call/stream 模式和 handler；不用分别注册 RTC/NATS 方法。
- 两个接入层验证各自凭据，再进入同一个授权与调用分发器；可信身份决定权限，传输名称不决定人/AI 身份或权限等级。
- 同一份 exec 命令目录驱动两通道调用、命令翻译、help、SDK 和校验；fs 保持内建能力。通道能力、设备状态与授权只过滤实际可用方法，不维护额外 caps.tools。
- 协议层只保留活动调用上下文、取消信号和流转发映射，不建立通用 session/operation/resource 注册表。exec 自己维护 bg 执行记录和输出；Browser/CUA 保留页面、窗口、快照、下载和输入状态，不重复管理同一次命令执行。
- `ui/1` 的既有词汇和描述可作为 browser/cua 的工具定义 presets；它不是第四个 aic-pod 标准协议。原 `browser/1` 的方法并入 browser 工具声明，不另起 browser/2 调用链。
- 首版不做工具 RPC daemon、动态插件系统或通用浏览器驱动系统。hosts_tools/1 首先是 Go 进程内契约；Chrome 仅实现自管 headless + pipe。

### 2. browser 内部状态模型

下列业务对象由 Browser service 管理；其中一次命令执行归 exec 内部管理，不发布到协议层通用资源注册表。

| 对象 | 责任与生命周期 |
|---|---|
| Browser service | 一个 backend 对应一个专用 Chrome 进程和持久 profile；懒启动、backend 退出回收 |
| Page `p_…` | 稳定的不透明页面实例 ID，AI/viewer 共用；关闭或 Chrome 重建后永不复用 |
| Observation `o_…` | 一次有界观察，包含页面、文档、动作版本及元素 refs；短期缓存 |
| 命令执行（exec 管理） | 普通短调用直接返回；可后台化的等待/编排由 exec 的 bg_* 管理，Browser 不重复保存执行状态 |
| Download `d_…` | browser 自管下载记录及受控暂存文件，与某次 click 和连接生命周期解耦 |
| Viewer / input activity | browser 管帧订阅、输入和自动控制状态；宿主只转发相应流 |

browser 自行分配不复用的 p_ ID，不同时暴露 windowId、nativeTarget、session-local target ID 和 surface_epoch。CDP targetId/sessionId 仅为内部映射。导航保留 page ID、更新 document ID，并由 browser 失效相关观察。

首版只有一个默认持久 profile，页面属于设备工作区，调用结束或传输断开不关闭页面。RTC/NATS 调用同一个 browser service；它根据宿主提供的可信 Caller 检查工作区/页面访问权限，无需先建立宿主逻辑 session。共享 profile 不承诺调用者之间的 cookie/数据隔离；需要隔离的不同主体使用独立 browser service/profile，暂不增加多 profile 管理指令。

**所有页面级调用显式传 `page_id`。** 删除服务端 `target use/current`。SDK 可持有 `Page` 对象，viewer 可本地选择标签；二者都只是把 ID 填进请求，不改变其他调用者的目标。

### 3. browser 的 hosts_tool 方法声明

browser 作为 exec 子命令一次声明下列方法，hosts_rtc/1 和 hosts_nats/1 共用 exec 的 command/method/args。CLI 由 aic-pod 按声明翻译，Browser handler 接收 typed 参数。斜线代表多个独立方法，不是万能 execute；浏览器专属参数属于方法 schema，不另定义 browser/2 信封。

| CLI 示例 | typed 方法 | 执行模型 |
|---|---|---|
| `browser status` | `status` | call：可用性、版本、方法与限制 |
| `browser pages` / `browser open URL` | `page.list` / `page.create` | call；open 返回 page ID |
| `browser navigate/back/forward/reload/close --page-id P` | `page.navigate/back/forward/reload/close` | call |
| `browser observe --page-id P [--image] [--query TEXT]` | `page.observe` | call：有界观察，可选图像、字段、范围、预算 |
| `browser click/fill/type/press/hover/scroll/drag/set --page-id P …` | `page.click/fill/type/press/hover/scroll/drag/set` | call；各动作有严格参数类型 |
| `browser wait --page-id P --text TEXT` | `page.wait` | call：可取消的有界等待，不占写队列 |
| `browser dialog accept/dismiss --page-id P --id D` | `page.dialog.resolve` | call；browser 控制路径，必须指定 dialog ID |
| `browser upload --page-id P --ref R --file PATH` | `page.upload` | call；browser 经共享文件策略取得获准的 file source |
| `browser downloads --page-id P` / `browser download get/wait/cancel/export D` | `download.list/get/wait/cancel/export` | call；export 单独授权 |
| 下载内容读取 | `download.read` | 有限范围 call；browser 校验访问权限并提供字节 |
| `browser events --page-id P --kind console,network --cursor C` | `page.events` | call：有界事件查询，含下一游标 |
| `browser eval --page-id P --code CODE` | `page.evaluate` | 高权限 call，不宣称任意 JS 只读 |
| `browser run --code CODE` | `script.run`（后续可选） | call；子调用逐步授权，整次流程由 exec 管理执行和输出 |
| viewer 观看 / 输入 | `page.frames`、`page.input` | stream；合法真实输入自动进入控制，空闲 10 秒退出，无接管按钮和 acquire/release call |

允许后台化的长等待/编排通过 exec 的 bg_list/bg_wait/bg_kill 管理；等待预算与执行期限分开，取消只停止这次执行，不关闭共享 Chrome。下载等独立业务对象仍由 Browser 管理；取消下载等待不等于取消下载。普通短方法不强制创建可恢复任务，也不提供协议层通用 operation.get/cancel。

规则：

- 合并 snapshot/screenshot 为 `observe` 的内容选项；`read/get` 变成带 locator 和 fields 的观察。只为新设计保留一个 CLI 拼法，不做旧命令别名。
- locator 是严格 one-of：ref、role/name、label、CSS、snapshot point；禁止混合多个 locator 后猜优先级。首版不扩大到复杂跨 frame locator DSL。
- `fill` 替换完整文本，`type` 追加到显式定位的控件，`press` 指定目标或明确 `focus:true`；无定位时不隐式沿用其他调用者的焦点。
- `set` 使用 typed value（checked、选项或数值），回读验证；输入动作成功仅表示已派发/已验证声明的条件，不表示站点业务一定成功。
- `wait` 使用具体 URL、文本、元素状态或 load 条件；不默认等待 network-idle，不无限等网络静默。等待前绑定文档/页面范围及 timeout。
- 写操作可显式请求 `after: none|summary|observation|image`，统一默认 summary（URL、标题、dialog、版本等轻量状态）；不每次点击强制全量 AX + 截图。后置观察失败保留动作效果并返回 warning。
- network/console/dialog/popup/download 使用一条有界事件时间线和 cursor，按 kind 过滤；不再共享一个会被其他调用者 clear 掉的日志。cursor 过期明确返回 gap，不伪装没有事件。

工具返回结构化领域结果，包括必要的业务句柄与读取方式。aic-pod 可按声明生成文本/图像呈现并限制输出预算，再由 RTC/NATS 适配器投递；不持有产物业务状态，不把已排版文本再解析回工具 service。两通道共用 call/result/fault；stream 仅通过 RTC 绑定，内部消息由前端与工具自行约定。

### 4. 请求、结果与脚本示例

两个通道信封承载同一个 hosts_tools/1 调用体。以下仅列公共业务调用；request_id、入场有效期属于信封，身份和 grants 来自可信调用上下文。不要求 session_id 或 operation_id；page_id、download_id 等业务引用属于命令参数，后台 execution_id 属于 exec 执行服务：

```json
{
  "domain": "exec",
  "command": "browser",
  "method": "page.click",
  "args": {
    "page_id": "p_…",
    "locator": {"role": "button", "name": "保存"},
    "timeout_ms": 5000,
    "after": "summary"
  }
}
```

工具产出一份领域数据，宿主作为这次 call 的结果经 hosts_rtc/1 或 hosts_nats/1 返回，随后释放应答关联：

```json
{
  "page_id": "p_…",
  "document_id": "doc_…",
  "revision": 12,
  "effect": "applied",
  "data": {"url": "https://example.com/settings"},
  "artifacts": [],
  "warnings": []
}
```

`effect=none|applied|partial|unknown` 是 browser 自己的结果语义，不是通用工具状态机。超时/取消可能伴随 unknown；观察失败不能把已派发动作改成 none。宿主未收到结果时只能报告调用失败或结果未知，不能推断动作未执行，也不能将 applied 描述成站点提交成功。

```text
browser open https://example.com                    # 返回 p_…
browser observe --page-id p_…
browser fill --page-id p_… --label 邮箱 --text me@example.com
browser click --page-id p_… --role button --name 保存
browser wait --page-id p_… --text 保存成功 --timeout 5s
```

```javascript
const page = await browser.open('https://example.com');
await page.fill({label: '邮箱'}, 'me@example.com');
await page.click({role: 'button', name: '保存'});
await page.wait({text: '保存成功', timeoutMs: 5000});
return await page.observe({image: true});
```

`run` 若保留，作为 exec 编排命令提供受限 Goja 执行。SDK 从命令声明生成 call，子调用回到共用授权分发器，不再 args→argv→parse；继承并收紧原授权，每步重新检查权限。整次执行及输出由 exec 管理，脚本内部保存步骤，不重复注册后台任务；不持整页大锁，无 Node/OS/CDP 裸接口，限制 CPU/时长/步数/输出，首版顺序执行。失败报告已完成步骤和部分效果，不承诺回滚或断线重跑。

### 5. Browser 自管页面调度，exec 管命令执行

页面状态、冲突写入队列、dialog 续行和下载等业务状态在 Browser 内；exec 仅维护命令执行记录、取消与输出。两个通道进入同一个 service，自然共享业务状态；协议层不解析 page_id 建立通用资源锁。

- browser 允许不同页面并行，串行化同页冲突写入，维护定位/输入/后置条件的业务边界。观察使用短检查点，不与本页写入乱序。
- 页面维护动作 revision：AI 操作及人工输入批次在派发前推进版本，失败/unknown 也不回退。版本用于识别受控写入，不能替代网页自身 DOM/布局变化检测。
- wait、下载传输、订阅帧不长期占用页面写槽；先注册事件游标/条件，再触发动作，避免快速事件丢失。
- 同步 dialog 只阻塞该页。`dialog.resolve`、关闭页面和查询状态进入 browser 控制路径，可在普通动作挂起时执行；其他页面继续工作。
- 普通 call 在预算内等待 handler 返回。页面事件可以暴露 dialog 等阻塞信息，由另一 call 处理；不为每次动作强制分配 operation ID。
- 长等待或流程若允许后台继续，由 exec 在入场授权后建立独立执行上下文，共用 bg_* 查询、取消与结果；等待超时只结束等待，不扩展授权。Browser 的独立业务对象仍按自己的规则存续，例如下载不因 download.wait 结束而停止。
- 超时/取消只通知停止，不代表副作用回滚。已经发出且结果不明时，browser 隔离该页未决写入，直到 CDP 收尾、显式关闭或 Chrome 终止；不能让宿主结束等待就触发下一条冲突写入。
- RTC/NATS 的认证、防重放归接入层；业务幂等若需要，由 browser 提供明确定义的 key、结果保留及冲突检查。request_id 仅用于应答关联，丢回复/重启后不自动重放动作，不承诺跨崩溃 exactly-once。

**容量约束：** 分发器保留有界的活动调用与流，但取消/关闭通知不能排在普通 handler 后面。browser 也必须给 dialog resolve/close 留出可执行的控制路径；若宿主实施方法并发限额，配置须允许这些方法在普通 browser 调用挂起时进入。无需为此引入通用 operation 状态机或资源调度器。

### 6. 观察与 ref：保守校验，但不全局失效

- 观察/ref 由 browser 创建并保存，绑定可信主体/访问范围、page/document、动作 revision 和采样时间。改变传输不改变主体身份；跨连接访问仍由 browser 检查，不能仅凭 ref 跨主体复用。另一个调用者的 observe 不会使其自动过期。
- 导航/执行上下文销毁、页面关闭、Chrome 重建使相关 ref 失效。DOM 中无关计时器变化不整页作废 refs。
- ref 保存 backend node identity 与必要语义指纹。执行前检查同一文档、节点仍连接、角色/标签与预期一致及可操作性；节点复用或无法确认时报 stale_ref，不自动改点相似元素。
- 语义 locator 在执行时解析、要求唯一匹配，并在预算内等待可见/可用；只重试定位和就绪检查，不重放已派发输入。
- AI 坐标动作必须携带图像观察、文档/动作版本和视口映射，限制观察年龄，检测滚动/布局变化；有疑问要求重新观察。全页图片仅供阅读，不直接当 viewport 坐标。
- 观察期间文档/版本改变时重采或返回不一致状态。网页可自行异步改变，不能声称 CDP 多条调用得到严格原子快照，或任何检查可以杜绝检查后变化。

### 7. 人工 viewer：输入驱动的自动控制

观看、建流不占控制权；前端直接发送输入，不暴露“接管/释放”步骤。browser 收到第一条合法真实输入时自动进入人工控制，后续真实输入续期，连续 10 秒无输入退出。输入通道不因空闲退出而关闭，下次输入自动恢复控制。

- 控制期间该页其他连接（包括 AI）的写入返回 control_busy，观察仍可用；其他页面不受影响。同一前端连接的导航由 browser 清理旧输入后执行。
- 输入先进入 browser 队列，已开始的 AI 写入结束后再执行；尚在排队的 AI 写入通过控制版本检查拒绝，避免输入交错。
- 同时仅一个人工 writer；控制持有者、活动时间、按键和鼠标状态归 browser。无心跳续租、对外 lease_id 或 acquire/release 方法；空闲清理按住状态后才重新允许 AI 写入。
- 失焦发送 browser 私有 reset 事件，重置按住状态而不进入或延长控制。隐藏、切页、断线关闭输入通道并清理；新通道不继承旧输入队列。
- 控制路径不绕过授权：AI 不能借 dialog/close 越过其他连接的人工控制；当前设备操作者可处理阻塞 dialog，或显式关闭页面。
- 空闲退出后 AI 重新观察，不自动执行之前被拒绝的动作。读帧不需要写权限，`page.frames` 与 `page.input` 分开授权。

browser 每页维护一个 screencast source 和多 viewer 订阅，共享 JPEG 字节并按 viewer 保留有界 latest 队列。前端接收独立于解码，也只保留一个最新待画帧。连续移动、滚轮在前后端均只保留未派发的最新值，不累加旧滚轮距离；点击、按键等离散事件保序。CDP ACK、坏帧处理和帧取舍归 browser；宿主只转发原始消息，不增加应用层 ACK/credit。慢 viewer 不阻塞其他人，固定设备视口不随 viewer resize 改变，静态页仅做有限次按需刷新，不持续轮询截图。专用 Chrome 关闭边界弹性回滚，防止顶部/底部反复拉伸图像。

### 8. 下载与文件：浏览器缓存和文件导出分离

不再把 download 定义为“先授权某个 tab，再由一次 click 触发落盘”。**默认允许页面下载到 browser 托管目录；读取/导出成为独立授权动作。** 这是新的产品语义，无需复刻旧 Electron 的逐 tab preventDefault。

- Chrome 启动时一次设置 context 级 `allowAndName` 和私有下载目录，默认不随每次命令开关下载策略；以 GUID 命名避免页面控制存储路径。该 API 作用于 context：[Browser 协议](https://raw.githubusercontent.com/ChromeDevTools/devtools-protocol/master/pdl/domains/Browser.pdl)。
- Download 属于来源 page/工作区，不猜“最近一次 click 的会话”。frame→page、popup opener 链用于记录来源；无法确认来源的下载不向普通调用者公开，取消并清理。
- browser 在 `download.list/get/wait` 中检查页面/下载访问权限；下载与发起调用或传输连接的生命周期解耦。点击结果可附期间观察到的下载事件，但不能把时间邻近当作唯一因果证据。
- 安装等待/事件游标后再触发按钮；多个候选下载返回列表或歧义，不默认拿第一个。SDK 的便利函数也只是组合这套调用。
- 完成后 browser 确认文件存在、大小、文件名和格式，并更新自己的下载记录；CDP completed 通知和 filePath 不能替代文件检查。`download.export` 经共享文件策略核准当前目标路径的写权限后提交。
- `download.read` 同样由 browser 校验访问权限，再通过 offset/limit 的有限范围 call 返回字节；d_ ID 不自带读取权。RTC/NATS 转发调用结果，不另建 artifact 注册表。raw profile/staging 路径不进入普通结果。
- upload 的本地路径由 browser 经共享文件策略取得已授权 file source，写入受控上传暂存再交给 CDP，不直接将客户端任意路径交给驱动。文件策略可共用，暂存和下载生命周期归 browser。
- 下载有总容量、单文件容量、并发、保留时间限制；超限取消并清理，启动清理未完成残留。CDP 事件配额不是精确磁盘硬上限；需要硬限制的部署使用独立卷/系统 quota。设备可配置全部 deny，但首版不声称支持原生按 tab 的严格零暂存授权。

这一设计避免 context 级策略竞争，也让 blob/POST/popup 下载仍由浏览器原生处理。具体来源事件与文件完成行为仍须实测。

### 9. Chrome、配置与能力发现

- 路径优先级：`browser_path > AIC_BROWSER_PATH > AIC_BROWSER_DEFAULT_PATH > PATH/系统已知路径`。用户显式路径无效就报错；desktop 注入值只是默认候选。
- 默认 path 必须是真正的 Chrome/Chromium，不是 Electron `process.execPath`。desktop 发布包独立携带浏览器才可保证开箱即用；不携带时依赖系统安装。参考：[Electron 打包入口](https://www.electronjs.org/docs/latest/tutorial/application-distribution)。
- 同一 Go launcher 管理 headless、静音、独立 profile、启动超时和进程树回收。pipe 的 POSIX fd 与 Windows 句柄分别实现，不自动回退到无鉴权 CDP 端口。参考：[Chromium pipe](https://chromium.googlesource.com/chromium/src/+/main/content/browser/devtools/devtools_pipe_handler.cc)、[Go ExtraFiles 限制](https://pkg.go.dev/os/exec#Cmd)。
- backend 装配时注册稳定的 browser service；status 暴露 `unavailable|stopped|starting|ready|failed` 与功能探测结果。注册表示实现了指令，不假装 Chrome 已启动；status/list 不为查询而启动浏览器，create 等需要引擎的方法 singleflight 懒启动。
- 崩溃后旧 page/ref/viewer ID 全部无效，记录明确失败/未知结果；有界重启仅恢复引擎，不恢复未决输入或自行重开业务页面。
- profile 锁防止多 backend 复用。启动后先订阅事件和配置下载/视口，再创建业务页面；不复用个人 Chrome 的默认数据目录。参考：[Chrome 调试目录要求](https://developer.chrome.com/blog/remote-debugging-port)。
- popup 由 Target 事件接管已有页面，由 browser 继承可确定的访问策略并分配新 p_ ID；不根据 windowOpen 再创建副本。外部协议、iframe、worker 不当作普通可操作 page，未知来源先隔离。参考：[Target 协议](https://raw.githubusercontent.com/ChromeDevTools/devtools-protocol/master/pdl/domains/Target.pdl)。
- 配置仅保留路径、默认视口和必要资源预算；首版不做远程 CDP、多 profile API、自动下载、引擎插件、自动登录态迁移。发行版本/架构、签名和 Docker 字体/运行库在打包阶段处理。

### 10. 代码组织与实施顺序

```text
protocol/hosts_tools/            公共工具声明、call/result/fault 与流契约
protocol/hosts_rtc/              RTC 接入与承载协议
protocol/hosts_nats/             NATS 接入与承载协议
libs/hosts_tool/                 工具声明与 typed handler binding
libs/hosts_tool/                 共用方法鉴权、call 派发、活动流转发
libs/exec_procs/                 复用进程/函数执行托管，扩展 typed result、bg_* 与独立输出
libs/host/                       通用接入、命令翻译、配置与装配
libs/browser/
  tool.go                       一次声明 browser 方法与 schema
  service.go / page.go           浏览器业务状态、页面与文档生命周期
  observe.go / actions.go        locator/ref、观察、真实操作和等待
  events.go / downloads.go       自管事件与下载记录，复用文件策略库
  live.go / input.go             viewer、控制租约、帧 source、输入 sink
  chrome/                       executable、OS launcher、pipe、CDP
libs/uiscript/                   可选编排工具，SDK 子调用回到共用分发器
```

每个工具不再拥有 browser_ai.go/browser_rtc.go 两套适配器；只有全局 RTC/NATS adapter。工具只声明一次 schema/方法/handler，通用分发器负责两通道接入。

2026-09-21 已完成 fs + exec 目录、后台执行和 FS/proxy 统一，剩余项为跨平台原生运行及可选脚本编排。

- [x] **1. 三协议与 hosts_tool 最小接口。** 固化声明、call、stream、方法权限与可信 Caller；明确取消/关闭行为，不要求通用 session/operation/resource。
- [x] **2. 共用分发器垂直验证。** 一个无状态 call 和一个简单流一次注册即可经 RTC/NATS 调用；验证授权、目录、CLI 翻译、超时取消和流清理。
- [ ] **3. 跨平台运行验收。** macOS Chrome 已实测、Windows/Linux 已交叉编译；仍需 三平台 pipe/生命周期、多页面视口与帧、静态 refresh、popup、blob/POST 下载和 profile 锁；确定发行物与预算。
- [x] **4. browser 工具实现。** 用 hosts_tool 完成 page.create/observe/click 垂直链路，再补工具内部的调度、locator/ref、wait/dialog、下载读取/导出；不先整套翻译旧 JS。
- [ ] **5. 可选脚本工具。** viewer 已接入并按真实输入自动控制；后续再提供 script 工具。 viewer 经同一目录调用 browser 自管的帧/输入接口；SDK 子调用回到共用分发器。验证输入自动进入、空闲退出、背压、取消和 dialog 解除阻塞。
- [x] **6. 装配与清理。** desktop 只传 Chrome 默认 path；更新服务器、前端、caps、签名测试向量、工具 SDK 和打包检查；删除旧 browser JS/TCP 桥、壳注册及各工具双入口分支。随后让 cua/其他工具使用同一注册接口，业务状态仍由各工具管理。
- [x] **7. 修正 fs + exec 能力模型。** 移除并列 caps.tools，Browser/CUA 等扩展直接注册为 exec.commands；前端 typed 方法与 AI 命令来自同一份声明。
- [x] **8. 统一执行与后台输出。** 复用 Start/StartTask，进程及服务方法共用 bg_*；分离等待和执行期限，保留 typed 结果、输出文件与取消事实，不把 service 生命周期纳入后台任务。
- [x] **9. FS、Proxy 和普通命令收口。** fs 保持内建身份，统一前端与 AI 文件实现；proxy 经 hosts_nats/1 转发；抽离鉴权并删除旧 hostcmd 的通用业务状态链。

验收按三协议和工具语义：一次注册、两通道可调用；无状态工具无需 session/operation；可信身份和业务对象访问不串用；回复丢失不自动重放；同页冲突写入串行、跨页并行；多页 dialog 不阻塞控制；接管无输入交错；ref/节点复用校验；坏帧/慢消费有界；下载访问、文件策略与配额；三平台完全独立于 Electron 的实际运行。

### 11. 明确删除的复杂度

删除工具各自的 RTC/NATS 方法表与适配器、重复的通道认证、强制的通用 session/operation/resource 系统、Go↔Electron TCP/代理、服务端当前标签、重复公开 ID、全浏览器大锁、跨命令塞续行结果、逐 tab 下载授权窗口和强制每步完整快照。工具自己的任务、资源访问、队列、缓存和租约按业务需要保留。

aic-pod 统一协议接入与调用机制；工具拥有业务状态和真实执行。工作量按分发与流转发、browser 行为和三平台验收估算，不按旧 JS 行数或简单协议字符串替换估算。
