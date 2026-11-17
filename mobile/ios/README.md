# ivec AI · iOS 壳

ivec AI 的 iOS 客户端。工作区（Agent 会话、技能、文件）由平台服务承载，壳是薄薄一层原生外壳：
把工作区稳稳托住、管好本地选择、把不可逆动作（同意协议 / 注销账户）说清楚。

| 项 | 值 |
|---|---|
| 应用名（商店名 = 备案名） | `ivec AI` |
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
│   ├── App/                     # 入口、根视图、常量
│   ├── Features/
│   │   ├── Workspace/           # 工作区（WKWebView）
│   │   ├── Settings/            # 设置（原生分组列表）
│   │   ├── Consent/             # 首启协议同意
│   │   └── Account/             # 注销账户
│   ├── Shared/                  # 主题、Keychain、服务地址、协议清单、ShellBridge
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
| 注销账号 | `DELETE /vb/api/auth/account` | Bearer 令牌；成功后清本机 Keychain 凭据 |
| 登出 | `POST /vb/api/auth/logout` | 仅吊销会话 |

网页登录后的令牌由页面通过 `window.webkit.messageHandlers.ivec.postMessage({type:"auth.token", token})`
交给壳，壳存进 Keychain（不进 UserDefaults、不写日志）。

## 隐私与权限

- 权限最小化：只申请麦克风（且仅在你使用语音输入 / 朗读时）。
- 壳不采集设备标识符（IDFA/OAID/IMEI）、不读通讯录与位置。
- 凭据存 Keychain；本地偏好（服务地址、主题、语言、同意记录）存 UserDefaults，
  已在 `PrivacyInfo.xcprivacy` 里声明 required reason（`CA92.1`）。
- 只用 HTTPS 标准加密：`ITSAppUsesNonExemptEncryption = false`，出口合规走豁免问卷。

## 开源约定

- 私钥、证书、Provisioning Profile 一律不入库（放在仓库根 `signing/`，已 gitignore）。
- 工程文件由脚本生成，**不建议手工编辑** `project.pbxproj`；改完源码重跑生成器。
