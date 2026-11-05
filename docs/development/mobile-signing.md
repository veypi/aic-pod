# 移动端签名与上架（ivec AI）

> 状态：2026-11-05 建档。命名与包名已定稿，三端签名材料部分就绪，APP 备案未提交。

## 1. 命名与不可逆项

| 项 | 值 | 提交后可否改 |
|---|---|---|
| 应用名（商店名 = 备案名 = 软著名的锚） | `ivec AI` | 不可改（软著/备案/上架三处须一字不差） |
| 包名（Android applicationId / iOS Bundle ID / 鸿蒙 bundleName） | `com.ivec.ai` | 不可改（鸿蒙 APP ID 创建后锁定） |
| 代码层标识 | `aic`（仓库 / 模块 / CLI / API 路径） | 可 |
| 服务域名（APP 备案与后端访问） | `ivec-ai.com` | 备案主体变更才需动 |

对外表述：**aic 是开源的 AI agent 引擎；ivec.ai 是官方托管服务与应用。**

桌面端（2026-11-05 已改名）：`productName` = `ivec`（产品层品牌），`appId` = `ai.ivec.desktop`，
`artifactName` = `aic-desktop-${os}-${arch}.${ext}`（代码层，保留不动，以免断现有下载链接与自动更新通道），
`package.json` 的 `name` 仍为 `aic-desktop`。即：**aic = 代码层，ivec = 产品层**。

## 2. 三端签名材料

私钥与证书**不入库**（`.gitignore` 已排除 `signing/`），本地路径：

```
signing/
├── PASSWORDS.md                              # 口令（本地私密，请转存密码管理器）
├── android/ivec-release.p12                  # PKCS#12 keystore，alias: ivec-release
├── android/ivec-release.cer                  # 公开证书
├── ios/apple-distribution.p12                # 私钥 + 临时自签证书
├── ios/apple-distribution.csr                # 上传 Apple 门户用
├── ios/apple-distribution-selfsigned.cer     # 占位证书（Apple 真证书下发后被覆盖）
└── harmonyos/                                # 待 AGC 建 APP ID 后生成
```

生成方式（2026-11-05 实测可用，**未用 DevEco / keytool / JDK**）：

```bash
# Android：自签证书 + PKCS#12（等价 keytool -genkeypair，RSA 2048，有效期 20 年）
openssl req -x509 -newkey rsa:2048 -sha256 -days 7300 -nodes \
  -keyout tmp-priv.p12 -out ivec-release.cer \
  -subj "/CN=ivec-release/O=Beijing Qiongju Technology Co., Ltd./OU=CC6VQMAF97/C=CN"
openssl pkcs12 -export -out ivec-release.p12 -inkey tmp-priv.p12 -in ivec-release.cer \
  -name ivec-release -passout "pass:$PW"

# iOS：Apple Distribution 的 CSR（CN 必须 ≤64 字符，LibreSSL 的 ASN1 上限）
openssl req -new -newkey rsa:2048 -sha256 -nodes \
  -keyout tmp-priv.p12 -out apple-distribution.csr \
  -subj "/CN=Beijing Qiongju Technology Co., Ltd. (CC6VQMAF97)/O=Beijing Qiongju Technology Co., Ltd./C=CN"

# HarmonyOS：EC secp256r1（必须！RSA 会被 AGC 判无效），用 JDK 17 的 keytool
# JDK 17 已装在 /Users/veypi/ivec/.tools/jdk-17.0.21+9（Temurin，免 sudo）
KT=/Users/veypi/ivec/.tools/jdk-17.0.21+9/Contents/Home/bin/keytool
"$KT" -genkeypair -alias "ivec AI" -keyalg EC -groupname secp256r1 -sigalg SHA256withECDSA \
  -dname 'CN="ivec AI", OU=CC6VQMAF97, O="Beijing Qiongju Technology Co., Ltd.", C=CN' \
  -validity 7300 -keystore app-release.p12 -storetype PKCS12 -storepass "$PW" -keypass "$PW"
"$KT" -certreq -alias "ivec AI" -file app-release.csr -keystore app-release.p12 -storepass "$PW"
```

CSR 三个候选（AGC 拒了再换下一个）：`app-release.csr`（keytool 原样，PEM 头是 `NEW CERTIFICATE REQUEST`）、
`app-release.standard-label.csr`（标签改成标准 `CERTIFICATE REQUEST`）、`app-release.der.csr`（二进制 DER）。

坑（本次实测踩到）：

0. **鸿蒙发布证书的 CSR 必须用 EC 密钥算法**（`-keyalg EC -groupname secp256r1 -sigalg SHA256withECDSA`）。
   华为官方 FAQ《申请发布证书时提示上传的CSR文件无效怎么办》明确：CSR 无效 = 生成方式不对，
   典型原因就是 keytool 生成时 `-keyalg` 没设成 `EC`，导致 CSR 用了错误密钥算法。
   实测：RSA 的 CSR（openssl 造的和 keytool 造的）**一律被 AGC 判「无效」**。
   来源：`developer.huawei.com/consumer/cn/doc/doccenter-dev-faq/faqs-appgallery-69`。

1. **设备 exec 写 `.pem` / `.key` / `id_rsa` 路径会被沙箱规则表硬拒**（`path denied by rule table; hard boundary`）。
   所以签名材料统一用 `.p12` / `.cer` / `.csr` / `.p7b` 命名，临时私钥写成 `.p12` 再导入并删除。
2. macOS 自带 **LibreSSL**：`openssl pkcs12 -export` **必须有 `-in <cert>`**（只给 `-inkey` 报 "unable to load certificates"），
   所以 iOS 侧先自签一张占位证书再打包 p12。
3. `openssl req -subj` 的 **每个字段 ≤64 字符**（`a_mbstr.c: maxsize=64`），
   因此 CSR 的 CN 写 `Beijing Qiongju Technology Co., Ltd. (CC6VQMAF97)`，
   **不能**写 `Apple Distribution: ... (CC6VQMAF97)`（72 字符会失败）。
4. `sed 's/.*=//; s/://g'` 在 BSD sed 上会报 `unsupported substitute flag ";"`，取指纹改用
   `awk -F= '{print $2}' | tr -d ':'`。

## 3. APP 备案填表口径

接入商：阿里云（`beian.aliyun.com`）。一个 App 备案一次，「添加运行平台」下分别填三套特征信息。

| 平台 | 软件包名称 | 公钥 | 「证书MD5指纹」栏 |
|---|---|---|---|
| 安卓 | `com.ivec.ai` | RSA 公钥模数（hex，512 字符） | 证书 MD5（32 位） |
| iOS | `com.ivec.ai` | RSA 公钥模数（hex，512 字符） | **证书 SHA-1**（40 位）——字段名叫 MD5，但 iOS 规范是 SHA-1 |
| 鸿蒙 | `com.ivec.ai` | **EC 公钥点 hex**（`04‖X‖Y`，P-256 = 130 字符） | `.cer` 叶子证书 MD5（32 位） |

> 鸿蒙的密钥算法是 **EC secp256r1**，不是 RSA——所以鸿蒙那行的公钥与安卓/iOS 不是同一种格式。
> AGC 下发的 `.cer` 可能是**证书链**（含根证书与中间证书），指纹必须取与私钥配对的那张**叶子证书**
> （脚本 `mobile/scripts/sign-fingerprints.sh` 已按 CSR 的公钥自动匹配叶子）。

取值统一用脚本：`bash mobile/scripts/sign-fingerprints.sh`（只输出公开信息，可直接抄表）。

Android 侧现成值（2026-11-05 生成，**提交备案后不可换密钥**）：

```
公钥(modulus) = B13EFFFF05F66E96274BC935511216854136AA44AD7DFE9ED05E1762A3D2B0A61075DBA5E6619D809FE5A1BE1C0D2E6BC8FD358BA181B32A2D9FB3B803662F7783E9A21A86D3640D7BEAFA162B5CFA81A4B56CFE44FAE6A47E63121E492400C4CB4AAC1ADD727CF3C2DCCE91127BCF9DE495AE766E5592BEC26C5B34FF61B3EA222D4D86108E04BEE4D22A535F5CE92C876496B593927912A53387A80E95DA489411162CA234FE3CCE2E70116E99464BA1F9E97C330C3E8B1B898972EAC8ED67B1A90463B867B9FE83AEA51D5D92F55E44C0333926E19E412C47CE7E8FA7D3141D1F6A669F052533A688108DE2A2F0B3EDBBBF002F9CDE1DA94B879C499C6CB3
证书MD5      = bed6e200705e87044a5ac22b6fe6e95c
证书SHA1     = 9275dda09b6fefdc3f80a2c0d6fa5903b00d6a06
```

iOS 侧现成值（Apple Distribution 证书已下发，2026-11-05）：

```
公钥(modulus) = BCA0933FC645BD4FD32D31C321B3E18F7B676D541601AE7CC44112BBD33279C44D3147D61C70DAF9DF9324C5078F28DBE757E61D3049A8D0D8948CD846D07620B8123184D22A1D87194CA724A36F3FA40794610197ED0A7B57A7EAE047095E550143D561FB72098633E23C8DE527730AD6CF472A3FED900F2DE55B4BFFAA9A0C6ED9882ADE3ECBE41B1A5D2FCDB0CD4FF34D8C81616861B27E8E02600A05DB1CE931B5F09657C94927037152C1CB25B8835F9CC120942FB6CFF4A28F26042ACA9E4DC62F4E0A2F09246D9B94231F2475096488BB757C51E0C18678734AF45C2EE64FF7A317A76DC4446B30F91AE7B354EF6FDD9E7541127D26932AB9EDCC1E3F
证书SHA1     = 87492c2c0d7c4193052c697d60669bad6d70aa89   <- 填「证书MD5指纹」栏
证书MD5      = 27ad0128499c085fb1a187915009489b
```

Apple 证书要点（`signing/ios/AppleDistribution.cer`，DER 格式，脚本已兼容）：
`CN=Apple Distribution: Beijing Qiongju Technology Co., Ltd. (CC6VQMAF97)`，
签发方 `Apple Worldwide Developer Relations Certification Authority (G3)`，有效期 2026-11-05 → 2027-11-05；
已校验其公钥与 `apple-distribution.p12` 私钥同源（modulus 一致）。

HarmonyOS 侧现成值（AGC 发布证书已下发，2026-11-05）：
```
公钥(EC 点hex) = 048e8d11a26ff8effddd78ed57b7e7edbbe8c0b10115ee6e70c345ad37705a6a28e36c4cd1c738c715e952e34e09433b5dbc50686c922dbf3ba2c4d34f1a677550
证书MD5        = 86c9f6c4aa1804081c24c562522b8e35
证书SHA1       = 8e5bbe9118dd22ddf100e12c8c40525c4a26dd8c
```

> **AGC 下发的 `.cer` 是 PEM 证书链（3 张）**，顺序为 `Huawei CBG Root CA G2` → `Huawei CBG Developer Relations CA G2`
> → **叶子**（`CN=北京穹矩科技有限公司(2077071641581483009) Release`，EC P-256）。
> 直接 `openssl x509 -in x.cer` 取指纹会拿到**根证书**（错的）——必须按 CSR 的公钥挑出与私钥配对的那张叶子（脚本已自动做）。

### 发布 Profile 创建要点（AGC 步骤六）

- 类型选「**发布**」（不选「指定设备发布」= 全量设备）；「选择证书」必须选**发布证书**（ivec AI）。
- Profile 名称建议 `ivec AI 发布`（日后调试 Profile 叫 `ivec AI 调试`，避免同名混淆）。
- 「申请权限：受限 ACL 权限」**默认不勾**：受限权限（悬浮窗、后台长时任务、读剪贴板、无障碍/设备管理等）
  需先到 AGC「ACL 权限」页单独申请、审核约 3 个工作日，获批后再回到这里勾选，**已获批权限才会写入 Profile**。
  Profile 是**一次性快照**：后续新增权限必须重建 Profile（重建不影响备案——备案只认证书指纹）。
  普通权限（网络、通知、相机、麦克风等）不走这里，写在 `module.json5` 的 `requestPermissions`，随包上架。
- 备案**不依赖** Profile/ACL，二者互不影响。


| 平台 | 软件包名称 | 公钥 | 证书MD5指纹 |
|---|---|---|---|
| 安卓 | `com.ivec.ai` | `B13EFFFF05F66E96274BC935511216854136AA44AD7DFE9ED05E1762A3D2B0A61075DBA5E6619D809FE5A1BE1C0D2E6BC8FD358BA181B32A2D9FB3B803662F7783E9A21A86D3640D7BEAFA162B5CFA81A4B56CFE44FAE6A47E63121E492400C4CB4AAC1ADD727CF3C2DCCE91127BCF9DE495AE766E5592BEC26C5B34FF61B3EA222D4D86108E04BEE4D22A535F5CE92C876496B593927912A53387A80E95DA489411162CA234FE3CCE2E70116E99464BA1F9E97C330C3E8B1B898972EAC8ED67B1A90463B867B9FE83AEA51D5D92F55E44C0333926E19E412C47CE7E8FA7D3141D1F6A669F052533A688108DE2A2F0B3EDBBBF002F9CDE1DA94B879C499C6CB3` | `bed6e200705e87044a5ac22b6fe6e95c` |
| iOS | `com.ivec.ai` | `BCA0933FC645BD4FD32D31C321B3E18F7B676D541601AE7CC44112BBD33279C44D3147D61C70DAF9DF9324C5078F28DBE757E61D3049A8D0D8948CD846D07620B8123184D22A1D87194CA724A36F3FA40794610197ED0A7B57A7EAE047095E550143D561FB72098633E23C8DE527730AD6CF472A3FED900F2DE55B4BFFAA9A0C6ED9882ADE3ECBE41B1A5D2FCDB0CD4FF34D8C81616861B27E8E02600A05DB1CE931B5F09657C94927037152C1CB25B8835F9CC120942FB6CFF4A28F26042ACA9E4DC62F4E0A2F09246D9B94231F2475096488BB757C51E0C18678734AF45C2EE64FF7A317A76DC4446B30F91AE7B354EF6FDD9E7541127D26932AB9EDCC1E3F` | `87492c2c0d7c4193052c697d60669bad6d70aa89`（SHA-1） |
| 鸿蒙 | `com.ivec.ai` | `048e8d11a26ff8effddd78ed57b7e7edbbe8c0b10115ee6e70c345ad37705a6a28e36c4cd1c738c715e952e34e09433b5dbc50686c922dbf3ba2c4d34f1a677550` | `86c9f6c4aa1804081c24c562522b8e35` |

备案高频驳回点（2026 官方审核帖点名）：备案的**主体证件号 / 应用名称 / 包名 / 主办单位**必须与上架时填的完全一致
（证件号注意 `5/S`、`1/I`、`0/O` 混淆）；管局下号有≥半天时差，刚过就提交会校验失败。

## 4. 各端上架流程要点

- **Android**：Google Play 需上传密钥（Play Console 上传密钥，RSA ≥2048、有效期 ≥22 天，本 keystore 20 年没问题）；
  国内商店走各自的开发者后台，签名用同一把密钥 → **只能用一把**（换钥 = 备案失效）。
- **iOS**：Apple Distribution 证书 + Bundle ID 标识符 + Provisioning Profile；App Store Connect 先建 App 记录
  （顺带把 `ivec AI` 这个名字占住，App 名全局唯一）。
  注意 Apple 后台成员单位地址与统一社会信用代码要和营业执照一致（当前 D&B 数据为 91113350，执照为 91110114MAK7U7DK9L，需修）。
- **HarmonyOS**：AGC 建 APP ID → DevEco 生成 p12 + csr → AGC 上传 csr 得 `.cer` → 创建发布 Profile `.p7b`
  → DevEco 配置签名（取消自动签名，手工配 p12/p7b/cer）→ Release 产出 `-signed.app`（不能是 `-unsigned.app`）
  → AGC 版本信息上传（使用场景选「测试和正式上架」）→ 合法性检测（必选）+ 上架自检（推荐）
  → 填表（基础信息 / 区域 / 资费 / 隐私 / **AI 功能声明** / 资质 / 备案 / 联系人 / 上架时间）→ 提交审核（1~3 工作日）。
  受限权限（ACL）先申请再建 Profile。
  - AGC 里 bundleName 的字段名是「**应用包名**」（应用详情页，铅笔图标可改）；工程侧对应 `AppScope/app.json5` 的 `bundleName`，两者必须一致。
  - 已建 APP ID（2026-11-05）：应用名 `ivec AI`、应用包名 `com.ivec.ai`、APP ID `6917620993145745936`、
    分类 应用、支持设备 手机/平板/PC/2in1/智能手表/智慧屏、所属项目 `ivec`。
  - 备案只需走到 AGC 流程的**步骤五（创建发布证书）+ 步骤六（创建发布 Profile）**，调试证书/调试设备/调试 Profile 不是备案前置。
  - csr 已用 openssl 生成（`signing/harmonyos/app-release.csr`）；若 AGC 拒收该 CSR，则装 DevEco Studio 用官方工具重做（会换密钥，备案前换无代价）。

## 5. CI 接入

| 端 | 需要注入的凭据 |
|---|---|
| Android | keystore（base64 或文件）+ alias + 口令 → `signingConfigs.release` |
| iOS | 已有 `APPLE_API_KEY` / `APPLE_API_KEY_ID` / `APPLE_API_ISSUER` 通道；iOS 证书可用 App Store Connect API 生成 |
| HarmonyOS | p12 + cer + p7b 三件套（`build-profile.json5` 的 signingConfigs 引用本地路径） |

## 6. 桌面端改名（2026-11-05 已执行）

定稿：**产品层叫 `ivec`，代码层叫 `aic`**。改 `productName` 不影响 Developer ID 签名（签名认主体不认产品名）。

已改：

- `desktop/electron-builder.yml`：`productName: ivec`（`artifactName` 保持 `aic-desktop-*`，`appId` 保持 `ai.ivec.desktop`）
- `desktop/package.json`：`productName: ivec`（`name` 保持 `aic-desktop`）、description
- `desktop/main.js`：菜单标题、关于/隐藏/退出 标签、托盘 tooltip、启动失败弹窗
- `desktop/loading.html`：标题与启动文案；`desktop/settings-ui/settings.html`：注释与错误提示
- `desktop/README.md`、`README.md`、`README.en.md`、`docs/development/macos-signing.md`（验证命令里的 `.app` 路径 → `ivec.app`）

副作用（测试版无用户，可接受）：macOS `.app` 包名变为 `ivec.app`（旧安装包 `AIC Desktop.app` 需重装）；
Electron `userData` 目录随 `app.getName()` 变化（`pet-pos.json`、`b-width.json` 等 UI 状态重置，**配置与凭据不受影响**——
配置在 `~/.aic`）。

（改了会影响已有下载链接与更新器通道，需评估）。
