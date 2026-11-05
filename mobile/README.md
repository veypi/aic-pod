# 移动端三端壳（ivec AI）

三端共用一套产品壳，承载 aic 平台能力（会话 / 技能 / 设备执行）。

| 端 | 目录 | 包名 / Bundle ID | 工具链 | 签名材料 |
|---|---|---|---|---|
| Android | `mobile/android/` | `com.ivec.ai` | Gradle + Android Studio | PKCS#12 keystore（`signing/android/ivec-release.p12`） |
| HarmonyOS | `mobile/harmonyos/` | `com.ivec.ai` | DevEco Studio（ArkTS / ArkUI） | p12 + cer + p7b（`signing/harmonyos/`） |
| iOS | `mobile/ios/` | `com.ivec.ai` | Xcode | Apple Distribution + Provisioning Profile（`signing/ios/`） |

## 命名约定（2026-11-05 定，提交后不可改）

- **应用名**（商店名 = 备案名 = 软著名的锚）：`ivec AI`
- **包名**（三端统一）：`com.ivec.ai`
- **代码层标识**：`aic`（仓库 / 模块 / CLI / API 路径）。`aic` = 开源引擎代号，`ivec` = 官方品牌
- **服务域名**：`ivec-ai.com`（已 ICP 备案，APP 备案与后端访问用）

一句话对外表述：**aic 是开源的 AI agent 引擎；ivec.ai 是官方托管服务与应用。**

## 签名材料

私钥与证书**一律不入库**，本地放在仓库根 `signing/`（已 gitignore），口令在 `signing/PASSWORDS.md`（本地私密，请转存密码管理器）。

取值脚本（输出可直接抄进备案表的三组值）：

```bash
bash mobile/scripts/sign-fingerprints.sh
```

## 当前状态（2026-11-05）

- [x] 命名与包名定稿（ivec AI / com.ivec.ai）
- [x] Android 上传密钥（PKCS#12，alias `ivec-release`）已生成
- [x] iOS Apple Distribution 私钥 + CSR 已生成（待上传 Apple 门户换正式证书）
- [ ] HarmonyOS：需先在 AGC 建 APP ID（`bundleName=com.ivec.ai`），再出发布证书 `.cer`
- [ ] 三端工程代码（待写）
- [ ] APP 备案提交（阿里云接入商系统，三端特征信息一次填齐）
