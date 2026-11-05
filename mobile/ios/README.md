# iOS 端（ivec AI）

工程尚未创建（2026-11-05）。前置条件（不依赖业务代码）：

1. App Store Connect 建 App 记录 → 顺带把 `ivec AI` 这个**全局唯一**的名字占住
2. Apple Developer 门户建标识符：Bundle ID = **`com.ivec.ai`**
3. 申请 **Apple Distribution** 证书：上传 `signing/ios/apple-distribution.csr`，
   下载后另存为 `signing/ios/AppleDistribution.cer`（真证书会覆盖 p12 里的临时自签证书）
4. `signing/ios/apple-distribution.p12` 导入钥匙串（口令在 `signing/PASSWORDS.md`）

账号事实（2026-11-05）：Apple Developer Program 组织账号已激活，Team ID `CC6VQMAF97`，
主体 Beijing Qiongju Technology Co., Ltd.，DUNS `576682284`。
**待办**：Apple 后台的成员单位地址与统一社会信用代码须与营业执照一致（当前 D&B 为 91113350，
执照为 91110114MAK7U7DK9L），App Store 上架前必须改，否则卡审核。

工程创建时锁死的项：Bundle ID、Team ID、Provisioning Profile（正式发布用 App Store profile）。

取值提醒：iOS 侧「公钥」= 证书的 RSA 公钥模数（hex），「证书MD5指纹」栏填 Apple 证书的 **SHA-1**（40 位，
字段名叫 MD5 但 iOS 规范就是 SHA-1）。用 `bash mobile/scripts/sign-fingerprints.sh` 取现成值。
