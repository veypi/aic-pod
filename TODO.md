# aic-pod 未来开发计划

- 文档状态：未来规划（只记未完成项）
- 说明：已完成的事实在 `CHANGELOG.md`，现行设计在 `docs/`。已归档的历史 todo（hosts-vsh-redesign 阶段 1-7 全部完成）随 2026-10-23 文档整理删除，git 历史可查。

## 待做

### 附属设备（attached device）—— 设计草案，未实现

2026-10-08 的草案（不持平台凭证、经主设备 SSH 中继接入的执行端）尚未落地：pod 代码里没有 `aic slave` 模式。草案已移出 `docs/`，归档在 `/Users/veypi/ivec/artifacts/hosts-attached/hosts-attached.md`；开工前需重新评审（依赖关系、凭证与 SSH 授权模型、平台侧 subject 与租约、平台侧落地时同步 `aic/docs/nats.md`）。

### 真实环境验收

- browser / CUA 真机（macOS / Windows）联调：画面通道、坐标缩放、权限诊断、退出清理。
- quota_provider 额度查询在真实账号上的余额 / 套餐语义核对。

### 待拍板 / 待确认（继承自 v0.18 大版本审查）

- 坏消息计数告警：gateway `next_cursor` 语义变更后坏消息被静默丢弃，无计数无告警（归属：平台侧 aic）。
- 部署环境是否设 `HTTP_PROXY`（web_search 行为变更的实际影响面；归属：运维）。
- `libs/endpoint` init() 双向校验 panic 在多副本滚动发布时的取舍：已定 fail-fast（起不来优于运行期故障），需写进运维文档。

## 维护纪律

- `docs/` 只保留现行设计；跨端契约改动要同步 `aic/docs`（反之亦然）。
- 各文档头部的「现行实现」日期随实现同步更新。
- 发版顺序与版本闸见 `CHANGELOG.md` 版本节；设备凭证迁移（v0.9.2 破坏性）见该节说明。
