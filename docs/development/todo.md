# aic-pod 未来开发计划

- 文档状态：未来规划（只记未完成项）
- 说明：已完成的事实在 `CHANGELOG.md`；已落地设计稿不留库（git 历史可查，如统一 commands 设计稿于 2026-11-02 删除）。已归档的历史 todo 随 2026-10-23 文档整理删除。

## 待做

### macOS 分发签名与公证（2026-11-05）

Desktop 的 mac 产物目前是 ad-hoc 深签（用户需在“隐私与安全性”手动放行）。Apple Developer
Program 组织会籍已付款，接 Developer ID 签名 + 公证 + CI 接入的完整方案见
[macos-signing.md](macos-signing.md)（含风险与退路、分阶段执行顺序）。

### 官方包预装后的收尾（2026-11-02）

browser/cua 已改为 pod 启动定版预装 + 发行包附带预编译 `bin/<name>`（见
`libs/host/command_preinstall.go`、`make skill-bin-*`），不再要求用户装 Go 工具链。尚待：

- **实机验收**：Desktop 安装包与 CLI 的 `skill-bin` 解包布局、预装命令注册与画面流；
  源码运行 pod（无附带二进制）的手动构建回退路径。
- **平台侧画面入口**：历史内置 Browser UI 的 `aic-browser` 标签生产零登记，前端默认
  值是否切换到 `<alias>:<stream>`（browser:view）需与前端确认。

### 远程 MCP 端点凭据（下一阶段）

URL 型命令目前不能携带自定义 header/token：mcpx 没有 HTTP 头注入口，manifest/配置也
刻意不接收 env/cwd（`validateHostCommand` 直接拒绝，不静默丢弃）。要接需要鉴权的远程
MCP 服务，需先设计 header/token 的配置面与权限边界。

### 统一命令与策略架构 —— 核心迁移已完成（2026-10-26）

现行契约见 [../architecture/overview.md](../architecture/overview.md) 与 [../reference/host-sandbox.md](../reference/host-sandbox.md)。统一 exec、按域配置快照、level/hard protection、普通名称与绝对路径、vbox 支撑命令、SSH/curl/git 内部策略及核心验收已经落地。

剩余工作：

- Linux / Windows 实机验证 FS/NET 后端、level 优先级、终局写根和命令行为；跨平台编译或 vet 不算行为验证。darwin 已在 mbp 的非嵌套沙箱环境完成真实 Seatbelt、双代理和工作区/hard protection 验收。

### 附属设备（attached device）—— 设计草案，未实现

2026-10-08 的草案（不持平台凭证、经主设备 SSH 中继接入的执行端）尚未落地：pod 代码里没有 `aic slave` 模式。草案已移出 `docs/`，归档在 `/Users/veypi/ivec/artifacts/hosts-attached/hosts-attached.md`；开工前需重新评审（依赖关系、凭证与 SSH 授权模型、平台侧 subject 与租约、平台侧落地时同步 `aic/docs/reference/nats.md`）。

### 真实环境验收

- browser / CUA Windows 真机联调：画面通道、坐标缩放、权限诊断、退出清理。mbp 的 browser 画面与人工操作曾在 v0.9.6-dirty 的**内置 browser 形态**下验收过；包形态（`commands install browser` + `browser:view` 通道）需重新验收，旧结论不算数。
- quota_provider 额度查询在真实账号上的余额 / 套餐语义核对。

### 待拍板 / 待确认（继承自 v0.18 大版本审查）

- 坏消息计数告警：gateway `next_cursor` 语义变更后坏消息被静默丢弃，无计数无告警（归属：平台侧 aic）。
- 部署环境是否设 `HTTP_PROXY`（web_search 行为变更的实际影响面；归属：运维）。
- `libs/endpoint` init() 双向校验 panic 在多副本滚动发布时的取舍：已定 fail-fast（起不来优于运行期故障），需写进运维文档。

## 维护纪律

- `docs/` 的目标设计与现行实现状态必须显式区分；实现完成后及时更新状态并删除 TODO 已完成项。
- Pod 文档负责总集成和跨仓接线；vsh/vbox 文档只描述各自契约，不在三个仓库重复写对方职责。
- 跨端契约改动要同步 `aic/docs`（反之亦然）。
- 发版顺序与版本闸见 `CHANGELOG.md` 版本节；设备凭证迁移见对应版本说明。
