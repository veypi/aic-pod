# aic-pod 文档地图

统一架构：根目录只放 `README.md` / `README.en.md` / `CHANGELOG.md` / `AGENTS.md`，
其余文档进 `docs/`，按 `architecture/`（架构设计）、`reference/`（契约规范）、
`guides/`（使用操作）、`development/`（计划与过程文档）分类。桌面壳文档留在 `desktop/`。

## 入口

| 文档 | 内容 | 何时读 |
| --- | --- | --- |
| [README.md](../README.md) / [README.en.md](../README.en.md) | 定位、安装、设备接入、能力边界 | 部署或使用 pod 前 |
| [AGENTS.md](../AGENTS.md) | 工程约定、构建测试、权限不变式、发版纪律 | 在本仓写代码前必读 |
| [CHANGELOG.md](../CHANGELOG.md) | 已完成变更（含生效条件：重启设备 / 重建桌面壳） | 发版与升级时 |
| [desktop/README.md](../desktop/README.md) | Electron 桌面壳：固定视口、窗口模型、打包 | 改桌面壳前 |

## architecture

| 文档 | 内容 | 何时读 |
| --- | --- | --- |
| [architecture/overview.md](architecture/overview.md) | 现行架构：统一命令模型、策略核心、模块划分 | 改执行/权限边界前建立整体图景 |

## reference

| 文档 | 内容 | 何时读 |
| --- | --- | --- |
| [reference/hosts-tools.md](reference/hosts-tools.md) | 设备执行、文件与工具服务的调用契约（协议、日志、MCP） | 改 exec/fs/协议面或接入调用方前必读 |
| [reference/host-sandbox.md](reference/host-sandbox.md) | host 执行权限与原生沙箱：规则域、grant、fail-closed 边界 | 改权限/沙箱前必读 |
| [reference/host-vtools.md](reference/host-vtools.md) | host-vtools：通用工具服务注册机制（mcp 管理面、流通道） | 新增设备侧服务或改注册层前 |

## guides

| 文档 | 内容 | 何时读 |
| --- | --- | --- |
| [guides/browser.md](guides/browser.md) | 设备侧 Browser 与 CUA 接入、实时画面通道 | 排查浏览器/画面问题前 |
| [guides/managed-ssh.md](guides/managed-ssh.md) | 托管 SSH：授权域、执行边界 | 使用或改 SSH 能力前 |
| [guides/managed-git.md](guides/managed-git.md) | 托管 git：ssh 传输子命令的授权与执行 | 使用或改 git 能力前 |
| [guides/release.md](guides/release.md) | 构建与发行边界：版本来源、桌面打包、运行时同步 | 发版时 |

## development

| 文档 | 内容 | 何时读 |
| --- | --- | --- |
| [development/todo.md](development/todo.md) | 未完成工作（实机验收、发布收尾） | 评估计划时；已完成事实写 CHANGELOG |
| [development/baselines/](development/baselines/) | browser 稳定性/性能基线数据（JSON） | 对比回归数据时 |

## 约定

- 契约真相只有一份：执行/工具看 `reference/hosts-tools.md`，权限看
  `reference/host-sandbox.md`，服务注册看 `reference/host-vtools.md`，其他文档引用而不复述。
- 代码注释里的 `docs/...` 指针必须指向现行路径，移动文档时同轮更新。
- 已完成的变更写 CHANGELOG（注明生效条件），未来计划只写 `development/todo.md`。
