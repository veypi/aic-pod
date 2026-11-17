# ivec AI · Android 壳

ivec AI 的 Android 客户端。工作区（Agent 会话、技能、文件）由平台服务承载，壳是薄薄一层原生外壳：
把工作区稳稳托住、管好本地选择、把不可逆动作（同意协议 / 注销账户）说清楚。

> 状态（2026-11-17）：阶段 5 第一版。已落地——统一 `window.aicHost` 桥、原生认证管理器（Keystore 加密存储 +
> 单一刷新执行者）、原生登录页与工作区 WebView 容器、文案单一真源（`client-bridge/l10n.json`）。
> 未落地——协议同意页与设置页、验证码登录、文件/分享/剪贴板/通知等原生能力（阶段 6）。
> **本机没有 Android SDK，以上代码未经过 Gradle 构建验证**，落地前先跑一次 `./gradlew assembleDebug`。

## 目录结构

```
mobile/android/
├── build.gradle.kts          # 插件与仓库
├── settings.gradle.kts
└── app/
    ├── build.gradle.kts       # 应用模块 + l10n 复制任务
    └── src/main/
        ├── AndroidManifest.xml
        ├── assets/l10n.json   # 构建时从 client-bridge/l10n.json 复制（不入库）
        ├── java/com/ivec/ai/
        │   ├── MainActivity.kt        # 登录页 ↔ 工作区 WebView
        │   ├── auth/AuthManager.kt    # 原生认证管理器（登录/刷新/登出/注销）
        │   ├── auth/AuthModels.kt     # 凭据模型 + JWT / Set-Cookie 解析
        │   ├── auth/CredentialStore.kt # Keystore(AES/GCM) 加密的登录档案
        │   ├── bridge/AicHostBridge.kt # window.aicHost 协议实现
        │   └── util/L10n.kt           # 文案读取（assets/l10n.json）
        └── res/layout/activity_main.xml
```

## 协议

桥协议与 iOS 壳、Desktop 同一份契约，见
[`docs/development/client-bridge.md`](../../docs/development/client-bridge.md) §七。

- 页面侧对象：`window.aicHost`（`version` / `platform` / `capabilities` / `call` / `on` / `off`）
- 页面 → 原生：`app_info`、`auth_state`、`auth_refresh`、`auth_sign_out`、`auth_open_login`、
  `account_delete`、`page_error`
- 原生 → 页面事件：`auth.changed`、`server.changed`、`navigate`
- 当前能力：`auth` 与 `accountDelete` 为真；文件 / 分享 / 剪贴板 / 通知 / 外部链接 / 深链 / 主题 / 语言为假（阶段 6）

## 构建

```bash
cd mobile/android
./gradlew assembleDebug
```

依赖 Google Maven 与 Maven Central；本机若无 SDK，先装 Android Studio 或命令行工具，
并在 `local.properties` 写 `sdk.dir=/path/to/Android/sdk`。
