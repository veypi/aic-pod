# aic-pod 未来开发计划

- 文档状态：未来规划（只记未完成项）
- 说明：已完成的事实在 `CHANGELOG.md`，目标设计在 `docs/`。已归档的历史 todo 随 2026-10-23 文档整理删除，git 历史可查。

## 待做

### 统一命令与策略架构 —— 核心迁移已完成（2026-10-26）

现行契约见 [../architecture/overview.md](../architecture/overview.md) 与 [../reference/host-sandbox.md](../reference/host-sandbox.md)。统一 exec、按域配置快照、level/hard protection、普通名称与绝对路径、vbox 支撑命令、SSH/curl/git 内部策略及核心验收已经落地。

剩余工作：

- vbox 的 Manager 缓存与 darwin 出站代理已实现；Pod 通过 `net_upstream_proxy` → `AuthCfg` → `compiledAuth` → net 快照显式下发 `Policy.Net.UpstreamProxy` 并纳入 netKey，不继承宿主 ambient proxy。真实 Seatbelt 验收按 vbox `docs/development/todo.md` 收尾。
- Linux / Windows 实机验证 FS/NET 后端、level 优先级、终局写根和命令行为；跨平台编译或 vet 不算行为验证。
- 最终发布收口：更新 CHANGELOG、必要的 `aic/docs` 契约同步与版本号确认。

### 附属设备（attached device）—— 设计草案，未实现

2026-10-08 的草案（不持平台凭证、经主设备 SSH 中继接入的执行端）尚未落地：pod 代码里没有 `aic slave` 模式。草案已移出 `docs/`，归档在 `/Users/veypi/ivec/artifacts/hosts-attached/hosts-attached.md`；开工前需重新评审（依赖关系、凭证与 SSH 授权模型、平台侧 subject 与租约、平台侧落地时同步 `aic/docs/reference/nats.md`）。

### 真实环境验收

- browser / CUA 真机（macOS / Windows）联调：画面通道、坐标缩放、权限诊断、退出清理。
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
