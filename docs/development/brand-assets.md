# 品牌与图标资产（ivec AI）

> 2026-11-05 建档。三端商店上架 + 桌面端改名后统一到「ivec AI」。

## 1. 色彩体系（2026-11-05 用户定，单色系）

| 角色 | 值 | 说明 |
|---|---|---|
| 形状 | **满幅出血的 squircle**（超椭圆 n=5） | macOS 图标惯例：**第三方图标不会自动加圆角**，圆角与边距必须自己烘进 PNG；留透明边距 → Dock 里明显显小 |
| 标记（icon） | **纯黑 `#000000`** | 描边式 logo，**只有黑白风格，无彩色版** |
| 底色（field） | **`#f1f8ff`** | **所有 logo / 图标都带这层底**，不透明（底色是底色，不算 logo 配色） |
| 暗色模式 | **CSS `filter: invert(1)`** | 整体反色 → 深底白标，暗色底上可见 |

原彩色/渐变版 logo 已废止——其中的深色段在暗色底上不可见（用户指出“内部 icon 根本不可见”）。

**不做透明底**：透明 SVG favicon 会被浏览器垫一层白底（Safari 尤其明显），且在不同背景上不可控；因此
favicon / 分享卡 / 网页 logo / 应用图标**共用同一份带底色的 SVG**。

## 2. Logo 源（唯一真源）

| 资产 | 路径 | 说明 |
|---|---|---|
| **平台 logo（矢量）** | `aic/ui/assets/logo.svg` | **唯一真源**：240×240，**满幅 squircle 底 `#f1f8ff`**（超椭圆 n=5，96 段 path）+ 黑色描边标记（开口圆环 + A 字，stroke-width 12） |
| 暗色反色规则 | `aic/ui/assets/global.css` | `:root[theme="dark"]` 与 `body[theme="dark"]` 两套选择器都覆盖 `img.brand-mark` / `.brand-mark img`（主题属性有两个落点：`root.html` 写 documentElement，`layout/theme.html` 写 body） |
| 网页引用 | favicon / og:image / twitter:image / JSON-LD logo / header / dock —— 全部指这一个 logo.svg | 只有一份，不分 favicon 版 |
| 桌面端托盘 | `aic-pod/desktop/assets/trayTemplate.png` / `@2x.png`（macOS 模板单色，系统自动反色）、`tray.png`（Linux/Windows，黑标记透明底） | 托盘是唯一保留透明底的场景（要叠在系统托盘上） |
| 桌面端应用图标 | `aic-pod/desktop/assets/icon.png` | 透明底 + 圆角 `#f1f8ff` 方块 + 黑标记 |
| 旧 Agent 六边形图标 | `aic-pod/desktop/assets/icon-agent-hexagon.png` | 保留备查（Agent 运行时专用视觉），勿用于商店 |

## 3. 图标包

生成器：`aic-pod/mobile/brand/render_ivec_mono.py`（PIL 按 logo.svg 几何重绘；改色/改尺寸直接重跑）。
产物：`aic-pod/mobile/brand/`

| 文件 | 用途 | 规格要点 |
|---|---|---|
| `ivec-app-icon-{1024,512,256,216,180,152,120,76,60,48,32,16}.png` | **商店主图标** | 满幅 `#f1f8ff` 方块 + 黑标记（标记占画布 62%）；**不透明、无 alpha、无自带圆角**（商店自己加圆角） |
| `ivec-desktop-icon-{1024,512,256}.png` | 桌面端应用图标 | **满幅出血 squircle**（n=5）+ `#f1f8ff` 底 + 黑标记（62%）；透明画布只留 squircle 外的四角 |
| `ivec-adaptive-fg-{432,1024}.png` | Android 自适应图标**前景层** | 透明底 + 黑标记，图形收在中心 66% 安全区（背景层用 `#f1f8ff`） |
| `ivec-mark-{1024,512,256,128}.png` | 透明底标记 | 托盘 / 深色底反色等特殊场景 |
| `ivec-tray-128.png` | 托盘图标 | 透明底 + 黑标记（已接入 `aic-pod/desktop/assets/tray.png`） |

**重跑方式**（mbp 上需 python3 + Pillow；系统自带 python3.9 带 PIL 11.3）：

```bash
python3 aic-pod/mobile/brand/render_ivec_mono.py <输出目录>
```

## 4. 生效边界（踩过的坑）

- **aic 4000（dev 模式 `debug=true`）**：静态资源（svg/css/png/组件 js）**磁盘直读**，改完刷新即生效；
  **平台壳 `ui/root.html` 是进程启动时读一次并缓存** → 改 root.html（含 favicon 引用）**必须重启 4000**。
- **浏览器 favicon 有独立缓存**：服务端换了图标 ≠ tab 换图标，必须刷新页面 / 重开 tab，顽固时清站点数据。
- `aic/init.go` 里 `//go:embed ui`：生产与 4002 是 embed 模式（改 UI 要重新编译），只有 dev 走磁盘直读。

## 5. 为什么自己用 PIL 重绘而不是直接转 SVG

mbp 上 **Chrome 不在 exec 白名单**（`denied: not in allowlist`）、`qlmanage` 在沙箱里起不来
（`sandbox initialization failed`）、无 `rsvg-convert`/`inkscape`/`ImageMagick`；
系统 `python3`（3.9.6）**带 Pillow 11.3.0** → 用 PIL 按 SVG 几何重绘最稳。
重绘保真度已对旧版 `tray.png`（平台 logo 原件）校验过：alpha 差异均值 10.2/255、轮廓 IoU 0.75、墨迹 bbox 偏差 ≤3px/128。

## 6. 命名口径（与 `mobile-signing.md` §1 一致）

- 应用名（商店 = 备案 = 软著）：**ivec AI**
- 包名（三端统一）：**com.ivec.ai**
- 代码层：**aic**；产品层：**ivec**；桌面端 `artifactName` 保持 `aic-desktop-*`
