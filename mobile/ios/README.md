# ivec ai · iOS 壳

ivec ai 的 iOS 客户端。工作区（Agent 会话、技能、文件）由平台服务承载，壳是薄薄一层原生外壳：
把工作区稳稳托住、管好本地选择、把不可逆动作（同意协议 / 注销账户）说清楚。

| 项 | 值 |
|---|---|
| 应用名（商店名 = 备案名） | `ivec ai` |
| Bundle ID | `com.ivec.ai` |
| 最低系统 | iOS 17.0 |
| 团队 | `CC6VQMAF97`（Beijing Qiongju Technology Co., Ltd.） |
| 许可证 | MIT（随 aic-pod 主仓开源） |
| 工作区地址 | `https://ivec-ai.com/u`（可在设置里改） |

## 目录结构

```
mobile/ios/
├── README.md
├── IvecAI.xcodeproj/            # 工程文件（由 scripts/gen-project.py 生成）
├── IvecAI/
│   ├── App/                     # 入口、根视图、总接线（AppCoordinator）、常量
│   ├── Features/
│   │   ├── Auth/                # 原生登录 / 注册页
│   │   ├── Workspace/           # 工作区（WKWebView）
│   │   ├── Settings/            # 设置（原生分组列表）
│   │   └── Consent/             # 首启协议同意
│   ├── Shared/                  # 主题、Keychain、服务地址、协议清单、宿主桥、原生认证
│   ├── Resources/               # Info.plist、PrivacyInfo.xcprivacy、Assets.xcassets
│   └── Supporting/
├── scripts/
│   ├── gen-project.py           # 生成 .xcodeproj（可重复执行）
│   └── build.sh                 # 构建 / 归档 / 跑模拟器
└── docs/APPSTORE.md             # 上架清单
```

## 构建

前置：Xcode 27.2+。本机 `xcode-select` 若仍指向 CommandLineTools，脚本用 `DEVELOPER_DIR` 直连
`/Applications/Xcode.app/Contents/Developer`，不改全局设置。

```bash
python3 scripts/gen-project.py        # 生成/刷新工程文件（源码增删后重跑）
bash scripts/build.sh                  # 模拟器 Debug 构建
bash scripts/build.sh run              # 装到 iPhone 18 Pro 模拟器并启动
bash scripts/build.sh device           # 真机 Release 归档（需签名）
```

在受限沙箱里跑 `xcodebuild` 时，Swift 宏插件需要 `-Xfrontend -disable-sandbox`；
`swiftc -typecheck` 可独立验证代码：

```bash
SDK=$(xcrun --sdk iphonesimulator --show-sdk-path)
xcrun swiftc -typecheck -Xfrontend -disable-sandbox -sdk "$SDK" \
  -target arm64-apple-ios17.0-simulator $(find IvecAI -name '*.swift')
```

## 与服务端的约定

| 用途 | 接口 | 说明 |
|---|---|---|
| 协议地址 | `GET /vb/api/config` | 读 `values.vb["auth.legal.terms_url"]`（用户协议）与 `values.vb["auth.legal.privacy_url"]`（隐私政策）；哪个配了就显示哪个 |
| 登录 / 注册 | `POST /vb/api/auth/login`、`/login/code`、`/register` | 令牌只从 HttpOnly Cookie 取；壳存进 Keychain（不进 UserDefaults、不写日志） |
| 刷新 | `POST /vb/api/auth/refresh` | 带 refresh cookie；原生管理器 singleflight，刷新后向 WebView 重新下发 cookie |
| 注销账号 | `DELETE /vb/api/auth/account` | Bearer 令牌；成功后清本机 Keychain 凭据 |
| 登出 | `POST /vb/api/auth/logout` | 仅吊销会话 |

壳是登录态的唯一所有者：原生认证管理器（`AuthManager.swift`）持有 access / refresh token，
WebView 里的 cookie 只是网页请求需要的镜像。网页不再把令牌交给壳，也不再自己刷新。

## 宿主桥 `window.aicHost`

网页与原生壳之间只认一个桥，协议见
[`docs/development/client-bridge.md`](../../docs/development/client-bridge.md) §七
（与 Desktop、Android 同一份契约）。壳注入 `window.aicHost`：`version` / `platform` /
`capabilities` / `call(method, params)` / `on(event, handler)` / `off(event, handler)`。

当前实现的方法：`app_info`、`auth_state`、`auth_refresh`、`auth_sign_out`、`auth_open_login`、
`account_delete`、`page_error`；事件：`auth.changed`、`server.changed`、`navigate`。
能力面当前只有 `auth` 与 `accountDelete` 为真（文件 / 分享 / 剪贴板 / 通知 / 外部链接 /
深链 / 主题 / 语言属阶段 6）。

工作区只留平台 origin：站外链接交给系统浏览器，原生桥不会注入到站外页面。

## 隐私与权限

- 权限最小化：只申请麦克风（且仅在你使用语音输入 / 朗读时）。
- 壳不采集设备标识符（IDFA/OAID/IMEI）、不读通讯录与位置。
- 凭据存 Keychain；本地偏好（服务地址、主题、语言、同意记录）存 UserDefaults，
  已在 `PrivacyInfo.xcprivacy` 里声明 required reason（`CA92.1`）。
- 文案单一真源是 `client-bridge/l10n.json`（构建时打进 App 包），壳内不再维护第二份文案表。
- 只用 HTTPS 标准加密：`ITSAppUsesNonExemptEncryption = false`，出口合规走豁免问卷。

## 开源约定

- 私钥、证书、Provisioning Profile 一律不入库（放在仓库根 `signing/`，已 gitignore）。
- 工程文件由脚本生成，**不建议手工编辑** `project.pbxproj`；改完源码重跑生成器。
