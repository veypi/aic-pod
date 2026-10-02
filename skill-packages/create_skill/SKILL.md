---
name: create_skill
version: 0.1.0
description: 创建 aic 技能的完整指南与模板：Q0 判别（L0 单文件组件 vs 技能包）、包结构契约、ui/api/tables/cli 四类能力模板、创建/编辑/发布/安装到设备全流程。用户要"做个页面/工具/技能/自动化"时必读。
keywords: [创建技能, 模板, 脚手架, skill, template, cli, ui, api, tables, 自动化]
icon: fa-solid fa-wand-magic-sparkles
---

# create_skill — 创建技能

用户说「帮我做个页面 / 工具 / 技能 / 自动化」时，先按 Q0 判别形态，再按本指南执行。**模板文件在 `templates/` 下，复制后改内容即可，不要从零写结构。**

## Q0 前置判别：要组件还是技能？

| 需求 | 形态 | 做法 |
|---|---|---|
| 只要一个前端页面/组件（展示、小工具、表单），不需要数据与命令 | **L0 单文件组件**（不建 skill） | 写单个 `.html` 到 page OPFS（临时）或 cloud UFS `/u/{uid}/`（长久），`page open /fs/{page\|cloud}/...html` 即开。vhtml 页面契约同样适用 |
| 需要持久数据（tables/api）、设备命令（cli）、多页应用、可发布分发 | **技能包** | 走下面完整流程 |

L0 限制：无 tables/api/cli 能力、无独立路由、不能发布到广场；需求一升级就转技能包。

## 技能包结构

```
{pkg}/
  SKILL.md            包身份 + 使用说明（frontmatter 契约见下，必需）
  ui/                 界面（vhtml 页面；index.html 缺省入口；langs.json 包 i18n）
  tables/             数据表定义（{table}.json）
  api/                数据接口（{method}.{name}.sqlx；begin/commit/rollback 事务文件）
  cli/manifest.json   命令清单（providers + streams；有 cli/ 即注册根命令 = 包名）
  cli/bin/            provider 可执行文件（安装时补执行位）
  scripts/            包内资源脚本（构建期打包，运行期经包目录相对路径调用）
```

四类能力（ui/api/tables/cli）**正交**：只做需要的，空目录不要留。SKILL.md frontmatter：

```yaml
---
name: my_tool          # 必需；^[a-z0-9][a-z0-9-_]{0,31}$；须等于注册表行 name
version: 0.1.0         # 发布/更新以注册表行为准；frontmatter 内保持一致
description: 一句话说清做什么（AI search 的主要命中面）
nickname: 显示名        # 可中文
keywords: [关键词, 数组]
icon: fa-solid fa-xxx  # Font Awesome
ui:                    # 有 ui/ 才声明
  - path: index.html
    desc: 页面说明
    handles: [md, txt]  # 可选：声明可打开的文件扩展/协议（OS open 通用机制）
---
```

## 创建与编辑（云端工作区）

1. **建行**：`POST /api/skills {name}` → 私有行 + `/skills/{id}/` 目录 + 初始 SKILL.md。响应带 `skill_id`（= 行 id）。
2. **写文件**：fs 门 `PUT /fs/cloud/skills/{id}/...`（私有行 owner 可读写；目录本身不可写——生命周期归注册表）。模板从 `/fs/cloud/skills/create_skill/templates/...` 读，改完 PUT 进去。
3. **改元数据**：直接改 SKILL.md frontmatter——私有行列表/详情**实时叠加** frontmatter（工作区单真相源，无 PATCH 端点）。
4. **调试**：页面 `/skills/cloud/{id}/`（详情页 `/skills_detail/cloud/{id}`，管理页 `/skills_admin/cloud/{id}`）；api 经 `POST /skills/cloud/{id}/api/{name}` 直接调。

## 能力契约速查

### tables（数据表）

`tables/{table}.json` 定义表（sqlite 运行库在包目录外，**发布不携带数据**）：

```json
{"fields": [
  {"name": "user_id", "type": "string"},
  {"name": "title", "type": "string", "required": true},
  {"name": "qty", "type": "int"}
]}
```

- `user_id` 是约定字段：每用户数据隔离（owner 管理界面见全量，api 执行器按访问者注入）。
- 管理面：`GET/POST /api/skills/{id}/tables/{table}`（owner-only）。

### api（sqlx 接口）

`api/{method}.{name}.sqlx` = 单条 SQL（`get.` 强制只读）：

```sql
-- api/post.add.sqlx
insert into items (user_id, title, qty) values (:user_id, :title, :qty)
-- api/get.list.sqlx
select title, qty from items where user_id = :user_id order by rowid
```

- `:user_id` / `:skill_id` 由服务端注入（调用方传入无效，防伪造）；参数 `:name` 来自 query/json/form。
- 调用：`POST /skills/cloud/{id}/api/add`（公开行任何人可调，私有行仅 owner）。
- 事务：`api/post.begin.sqlx`（内容 `BEGIN`）/ `.commit.sqlx` / `.rollback.sqlx`。
- 原始 SQL（owner）：`POST /api/skills/{id}/tables_sqlx`（白名单读写分流）。

### ui（vhtml 页面）

- 页面放 `ui/`，缺省入口 `ui/index.html`；页面地址 `/skills/cloud/{id}/{page}`（剥 .html，index 省略）。
- 平台固定 `env.js`（作者不可自携/覆盖）：loader `import {scoped}/env.js` 完成模块引导；`{scoped}/langs.json` 映射包 `ui/langs.json` 并入共享 i18n。
- 读平台数据用 `$fetch`（模块本地，与根环境同契约）；调自己的 api 用相对地址 `api/{name}`。
- `handles` 声明后可被 OS `open <文件|URL>` 通用打开（如 browser 注册 http/https）。

### cli（设备命令）

`cli/manifest.json`：

```json
{
  "providers": [
    {"id": "main", "kind": "process", "entry": "cli/bin/hello"},
    {"id": "svc", "kind": "service", "entry": "cli/bin/hello-service"}
  ],
  "streams": [{"name": "events", "provider": "svc"}]
}
```

- **process**：每次调用起一个进程跑 entry（argv 全量传入）——无状态命令脚本即可（shell/python 都行，模板是 shell）。
- **service**：常驻进程，skillrun 懒启动（首调用）+ bg 登记；经 `SKILLPROC_SOCKET`（env 注入的 unix socket）收 invoke/stream 帧——有状态（持连接/会话）才需要，协议参考 browser/cua 包源码。
- 安装到设备：`skill download <ref>`（pod vsh 指令；ref = 注册表 id 或本人私有行 name）→ 解包 `~/.aic/skills/{name}/` → 校验 manifest + 补执行位 + 注册根命令（= 包名）。之后 AI 与人都可用 `{name} <args>`。
- 大二进制不进包：用 `artifacts.lock.json`（来源/摘要/平台三元组）在安装阶段设备侧下载校验。
- 根命令冲突（内建/保留名/已装包）download 时显式拒绝；禁用 = 命令保留但显式报错，不回落同名系统程序。

## 发布与分发

1. **发布**：管理页或 `POST /api/skills/releases {source, name?, version}`（source = 私有行 id/name；name 省略 = 公开占 source 名）。流程：契约校验（frontmatter name == 行 name；有 cli/ 校验 manifest）→ 配额（公开 ≤10）→ 打包（>16MB 拒）→ 审核（全量审核，无免审通道）→ 通过后目录**冻结只读**。
2. **公开行不可变**：无编辑面 + fs 门只读双保险；改内容 = 发新版本（版本号一经提交即消耗，重提须递增）。
3. **fork**：`POST /api/skills/{id}/copy`（或 cloud vsh `skill download <ref>`）→ 得 caller 私有副本（可改可再发布）。
4. **安装到设备**：详情页「安装到设备」按钮或 `exec(host, "skill download <ref>")`。

## 模板索引（复制即用）

| 模板 | 说明 |
|---|---|
| `templates/ui/index.html` | 最小 vhtml 页面（env.js 引导 + $fetch 调包内 api 示例） |
| `templates/tables/items.json` | 数据表（user_id 隔离约定 + required 示例） |
| `templates/api/post.add.sqlx`、`templates/api/get.list.sqlx` | 增/查接口 |
| `templates/cli/manifest.json` + `templates/cli/bin/hello` | 最小 process provider（shell 脚本：解析 argv、JSON 输出契约） |
| `templates/scripts/` | 包内资源脚本骨架（随包分发，运行期相对包目录调用） |

## 寻址与 fs 门（写代码时最易踩）

- **系统面严格 id**：URL/API 路径参数一律用注册表 id；`name` 只是 AI 工具调用的便利解析（caller 自己的行：私有先、公开后，再内建）。
- `/fs/cloud/skills/`（L1）不可列不可读；`/skills/{id}` 目录写恒拒；公开行内容只读；私有行 owner 可读写、他人 404（不暴露存在性）。
- 单段路径名 >64 字符直接报错（连锁约束：name ≤32、version ≤16）。
- 响应里的 `url_prefix` 字段（= `/skills/cloud/{id}`）：拼地址一律 `{url_prefix}/...`，不要硬编码前缀。
