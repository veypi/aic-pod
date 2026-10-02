---
name: cua
version: 0.1.0
description: 设备原生桌面自动化（Computer Use）：窗口/应用枚举与激活、原生控件观察（无障碍树+截图）、点击/输入/拖拽/菜单/剪贴板操作。驱动本机 CuaDriver（macOS 辅助功能 API），供 AI 操作桌面应用。
ui:
  - path: index.html
    desc: CUA 状态与授权诊断（驱动状态 + macOS 辅助功能/屏幕录制授权指引）
---

# cua

设备原生桌面自动化。根命令 `cua`，全部操作走子命令；驱动 = 本机 CuaDriver（MCP over stdio + darwin daemon），设备级单会话——同设备所有调用共享同一份窗口绑定与快照令牌。

## 用法

```
cua <subcommand> [args] [--json]
```

输出契约：stdout 只放约定 JSON（`--json` 紧凑单行，默认缩进）；诊断写 stderr。非零退出不能当成功数据使用。

### 子命令

| 命令 | 说明 |
| --- | --- |
| `status` | 驱动状态（state=ready/stopped/unavailable + 驱动路径） |
| `app.list` / `app.open <app>` | 列出 / 打开应用 |
| `window.list [--app A] [--pid N]` | 列出窗口（返回 `window_id`） |
| `window.observe <window_id> [--query Q] [--depth N] [--image]` | 观察窗口：无障碍元素树（含 `ref`）+ 可选截图 |
| `window.activate <window_id>` | 激活窗口（不需审批，由 rules 决定） |
| `window.menu <window_id> --path a,b,c` | 触发菜单路径（≤16 段） |
| `window.bounds <window_id> --x N --y N --width N --height N` | 调整窗口位置尺寸 |
| `window.wait <window_id> [--text T \| --role R --name N --state S]` | 等待文本出现或语义 locator 状态 |
| `window.<click\|fill\|type\|press\|scroll\|set\|move> <window_id> <locator-flags> [选项]` | 窗口动作：`--text T` `--key K` `--value V` `--button left\|right\|middle` `--count 2` `--delivery background\|foreground` `--after none\|observation\|image` |
| `window.drag <window_id> --snapshot S --from x,y --to x,y [--delivery ...]` | 基于截图坐标拖拽 |
| `clipboard.read` / `clipboard.write <text>` | 剪贴板 |
| `cursor.state` / `cursor.set --enabled[=false]` | 光标状态/显隐 |
| `observation.image.read <window_id> <image_id> [--offset N] [--limit N]` | 分块读取观察截图（≤32KB/次） |

locator flags：`--ref R`（observe 返回的元素引用；窗口变化/新快照即失效）| `--role R --name N` | `--label L` | `--snapshot S --at x,y`（截图坐标）。

实证契约：press 的 `--key` 修饰键用 `Meta/Control/Alt/Shift`（如 `Meta+q`；`cmd+q` 报 invalid key modifier），且 press 必须带 locator；click/press 后台投递返回 `effect=unverifiable` 属常态（不回读），效果靠 observe/截图核验；`observation.image.read` 的 offset 单位是解码后字节，分块拼到 `eof=true` 即完整图片。

### 典型流程

1. `cua status --json` 确认驱动 ready（unavailable = 未装/未授权，见下）。
2. `cua app.open Safari` → `cua window.list --app Safari` 拿 `window_id`。
3. `cua window.observe <window_id>` 拿元素 `ref`；`window.click/fill/type --ref ...` 操作。
4. 需要截图观察时 `window.observe --image` → `observation.image.read` 分块读图。

错误语义：`session_expired`（驱动会话断了，下一条命令自动恢复，需重新枚举绑定目标）；`stale_ref`（ref 属于旧快照，重新 observe）；`unavailable`（驱动缺失或未授权）。

### 授权（macOS）

CuaDriver 使用辅助功能（Accessibility）与屏幕录制（Screen Recording）权限，授权身份归 `com.trycua.driver`（签名/公证原样随桌面端发行）。首次操作被系统拒绝时：系统设置 → 隐私与安全性 → 辅助功能 / 屏幕录制 → 启用 CuaDriver；或命令行 `cua-driver doctor` 诊断、`cua-driver permissions grant` 引导授权。诊断 UI 见包页面（状态与授权指引）。

驱动探测链：`AIC_CUA_DRIVER_PATH`（显式覆盖）→ `AIC_CUA_BUNDLE_DIR`（桌面端内置发行物目录提示）→ PATH → 系统安装路径（`~/.local/bin`、`/Applications/CuaDriver.app`、`/usr/local/bin`、`/opt/homebrew/bin`）。
