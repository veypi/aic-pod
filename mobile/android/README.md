# Android 端（ivec AI）

工程尚未创建（2026-11-05）。创建时必须锁死的项：

- `applicationId` = **`com.ivec.ai`**（与 iOS / 鸿蒙一致；APP 备案已按此值填，提交后不可改）
- 签名：`signingConfigs.release` 从 `mobile/signing.env` 读 `AIC_ANDROID_KEYSTORE` / `AIC_ANDROID_KEY_ALIAS` / `AIC_ANDROID_KEYSTORE_PASSWORD`，
  keystore 为 PKCS#12（`signing/android/ivec-release.p12`，alias `ivec-release`）
- 不得把 keystore 或口令写进代码/仓库（`.gitignore` 已排除 `signing/`、`*.jks`、`*.p12`、`*.keystore`）
- 出包必须用 **release 签名**构建（调试签名与备案指纹不一致，上架/备案校验会失败）

取值提醒：Android 侧「公钥」= 签名证书的 RSA 公钥模数（hex 512 字符），「证书MD5指纹」= 证书 MD5（32 位）。
用 `bash mobile/scripts/sign-fingerprints.sh` 取现成值。
