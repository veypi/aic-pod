---
name: browser
version: 0.1.0
description: 设备浏览器能力（page.* 页面自动化 + download.* 下载管理 + page.frames/page.input 实时流）。驱动本机 Chrome，供 AI 浏览、观察与操作网页。
ui:
  - path: index.html
    desc: 设备浏览器查看器（页面列表 + 实时画面 + 输入转发；多标签 open 指令 + ?url= 深链）
    handles: [http, https]
---

# browser

设备上的 Chrome 浏览器。根命令 `browser`，全部页面操作走子命令；实时画面与输入走 stream 端点。

## 用法

```
browser <subcommand> [args] [--json]
```

输出契约：stdout 只放约定 JSON（`--json` 紧凑单行，默认缩进）；诊断与警告写 stderr。非零退出不能当成功数据使用。

### 子命令

| 命令 | 说明 |
| --- | --- |
| `status` | 浏览器服务状态（state/executable/viewport/error） |
| `page.list`（别名 `pages`） | 列出页面 |
| `page.create [url]`（别名 `open`） | 新建页面（`--width N` `--height N`） |
| `page.navigate <page_id> <url>`（别名 `navigate`） | 导航 |
| `page.close <page_id>`（别名 `close`） | 关闭页面 |
| `page.observe <page_id>`（别名 `observe`） | 观察页面：元素树 + 可选截图（`--query Q` `--limit N` `--image`）。返回元素 `ref` 供后续动作定位 |
| `page.wait <page_id>`（别名 `wait`） | 等待条件：`--text T` / `--url U` / `--load` / locator + `--state visible\|hidden\|enabled`；`--timeout_ms N`（≤300000） |
| `page.events <page_id>` | 页面事件流（`--cursor N` `--kind K`；navigation/dialog/console/network/popup） |
| `page.dialog.resolve <page_id> <dialog_id>` | 处理 JS 对话框（`--accept` / `--accept=false` `--text T`） |
| `page.evaluate <page_id> <code...>`（别名 `eval`） | 执行 JS，返回 returnByValue 结果 |
| `page.upload <page_id> <locator-flags> <file>` | 给 file input 上传本地文件 |
| `page.<click\|fill\|type\|press\|hover\|scroll\|drag\|set> <page_id> <locator-flags> [选项]` | 页面动作：`--text T` `--key K` `--value V` `--x N --y N` `--after none\|summary\|observation\|image` |
| `page.<back\|forward\|reload> <page_id>` | 历史导航 |
| `download.list <page_id>`（别名 `downloads`） | 页面下载列表 |
| `download.get / download.wait / download.cancel <download_id>` | 下载状态/等待/取消 |
| `download.export <download_id> <path>` | 导出到本地路径（目标不存在才写，相对路径按调用 cwd 解析） |
| `download.read <download_id>` | 读取下载内容（`--offset N` `--limit N`，≤32KB/次） |

locator flags 四选一：`--ref R`（observe 返回的元素引用；页面导航即失效，单页引用满 4096 条时淘汰最久未用）| `--css C`（必须唯一匹配）| `--role R --name N` | `--label L`。

### 典型流程

1. `browser page.create https://example.com --json` → 拿 `page_id` 与 `document_id`。
2. `browser page.observe <page_id>` → 拿元素 `ref`。
3. `browser page.click <page_id> --ref ref_x`；`page.fill --ref ... --text ...`；`page.wait --load`。
4. 下载：`download.list <page_id>` → `download.wait <download_id>` → `download.export <download_id> ./file.pdf`（相对路径按你的 cwd 解析）或 `download.read` 直接读内容。

注意：页面被真实用户输入（page.input 租约）占用时，自动化动作返回 `control_busy`；页面关闭后其 page_id、ref 与下载记录（含已下载文件）全部回收失效。

### stream 端点（RTC 私有，不在 CLI 面）

- `page.frames`：页面实时画面（JPEG 帧流，只读）。
- `page.input`：真实输入通道（指针/键盘事件批，占用页面控制租约）。

## 配置（包内默认 + 环境变量覆盖）

- `AIC_BROWSER_PATH`：Chrome 可执行文件（显式用户覆盖，最高优先）。缺省探测链：`AIC_BROWSER_BUNDLE_DIR`（打包器提示：目录内含 Chrome for Testing，`{platform}-{arch}/` 布局，desktop 随包分发）→ 系统候选（macOS .app / Windows PROGRAMFILES 系 / Linux PATH 名）→ 报错提示 set `AIC_BROWSER_PATH`。
- `AIC_BROWSER_STATE_DIR`：状态目录（默认 `$HOME/.aic/browser`）。
- `AIC_BROWSER_WIDTH` / `AIC_BROWSER_HEIGHT`：新建页面默认视口（默认 1280/720）。

打包形态：desktop 把本包 `build.sh` 产出的 `browser.zip` 带进 resources，pod 首跑经 `AIC_BUILTIN_SKILLS` 预装（builtin 来源；幂等，失败不阻断启动）。
