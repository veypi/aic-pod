# HarmonyOS 端（ivec AI）

工程尚未创建（2026-11-05）。前置条件（**都不依赖业务代码**，可先做）：

1. AGC 建 APP ID：`bundleName` = **`com.ivec.ai`**（创建后不可改），应用名 `ivec AI`
2. DevEco Studio `Build > Generate Key and CSR` → `app-release.p12` + `app-release.csr`（CSR 内不要中文）
3. AGC「证书管理」上传 csr → 下载发布证书 `.cer` → 放 `signing/harmonyos/app-release.cer`
4. AGC 创建**发布 Profile** → `app-release.p7b` → 放 `signing/harmonyos/app-release.p7b`
5. 工程内 `AppScope/app.json5` 的 `bundleName` 与 APP ID 一致；`build-profile.json5` 的 `signingConfigs`
   取消自动签名，手工配 p12 / p7b / cer 三个路径

工程创建时锁死的项：bundleName、签名三件套路径、Stage 模型 `bundleType: app`。

发布产物必须是 Release 构建的 **`-signed.app`**（`build/outputs/default/` 下；`-unsigned.app` 会被 AGC 直接拒收），
上传时「使用场景」选「测试和正式上架」。

取值提醒：鸿蒙侧「公钥」= `.cer` 里的 RSA 公钥模数（hex），「证书MD5指纹」= `.cer` **叶子证书**的 MD5（32 位；
`.cer` 若含根证书+中间证书需先剥到叶子）。用 `bash mobile/scripts/sign-fingerprints.sh` 取现成值。
