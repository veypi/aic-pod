# macOS 分发签名与公证（Developer ID）

- 文档状态：**代码已落地（2026-11-05），待 CI 首次跑通验证**。现状与契约见 [../guides/release.md](../guides/release.md)、[../../desktop/README.md](../../desktop/README.md)。
- 目标：aic-desktop 的 macOS 产物用 Developer ID Application 证书签名并经 Apple 公证（notarization）+ staple，用户双击打开不再被 Gatekeeper 拦截；签名与公证在 GitHub Actions 内完成。
- 不在本文范围：App Store 上架、iOS 分发、macOS 自动更新（依赖本文完成后另立）。

## 现状（实施前的事实）

| 事实 | 位置 |
| --- | --- |
| mac 段不写死 identity：electron-builder 在钥匙串找**有效**的 Developer ID Application 身份（CI 由 `CSC_LINK` 导入临时钥匙串）；无有效身份时整体跳过签名（无自动 ad-hoc 回退），由 afterPack 钩子走 ad-hoc 兜底 | [../../desktop/electron-builder.yml](../../desktop/electron-builder.yml) |
| afterPack 钩子：校验随包运行依赖后按可用证书分流——有 Developer ID 身份则给 `Contents/Resources` 下的裸 Mach-O 补签（osx-sign 不签 Resources），无则 ad-hoc 深签 `codesign --force --deep --sign -` | [../../desktop/scripts/after-pack-sign.js](../../desktop/scripts/after-pack-sign.js) |
| ad-hoc 是社区 workaround：electron-builder 26 直接 ad-hoc 会破坏摄像头/麦克风（electron-builder#9529） | 同上文件注释 |
| 打包前由 beforePack 钩子同步运行依赖到 `desktop/vendor/`（Chrome for Testing / agent-browser / CuaDriver，按版本 + sha256 钉版） | [../../desktop/scripts/before-pack-browser.cjs](../../desktop/scripts/before-pack-browser.cjs) |
| `desktop/vendor/` 被根 `.gitignore` 的 `vendor/` 忽略 → CI 上现下现取 | [.gitignore](../../.gitignore) |
| CI desktop job 已在 macos-15(arm64) / macos-14(x64) 跑 `make desktop-darwin-*`，打 tag 发 GitHub Release | [.github/workflows/build.yml](../../.github/workflows/build.yml) |
| 随包 Mach-O：`resources/backend/aic-backend`（Go）、`resources/agent-browser/<os>-<arch>/`（官方原生 CLI）、`resources/skill-bin/`（预编译 browser/cua 设备程序）；`resources/browser/` 内是 Google 签名的 Chrome for Testing.app，`resources/cua/` 内是上游签名的 CuaDriver.app | electron-builder.yml 的 `extraResources` |

electron-builder 26 的签名/公证语义（已读 `node_modules/app-builder-lib` 源码确认）：

- `mac.identity`：**不设** → 在钥匙串里找有效证书（找不到就整体不签，**没有自动 ad-hoc 回退**）；`null` → 完全跳过；`"-"` → 显式 ad-hoc。
- `mac.hardenedRuntime` 默认 **true**。
- `mac.entitlements` 不设时自动使用 `build/entitlements.mac.plist`；子进程继承用 `build/entitlements.mac.inherit.plist`。
- `mac.notarize` 是**布尔开关**（`undefined` = 开启，无凭据即跳过，`false` = 显式跳过）。公证凭据只从环境变量取，三选一：
  1. `APPLE_API_KEY` + `APPLE_API_KEY_ID` + `APPLE_API_ISSUER`（推荐）
  2. `APPLE_ID` + `APPLE_APP_SPECIFIC_PASSWORD` + `APPLE_TEAM_ID`
  3. `APPLE_KEYCHAIN` + `APPLE_KEYCHAIN_PROFILE`
- 签名凭据：`CSC_LINK`（p12 的 base64 或路径）+ `CSC_KEY_PASSWORD`；`CSC_NAME` 指定证书名。

## 方案

签名（electron-builder 自带签名）→ 补签嵌套裸 Mach-O（afterSign 钩子）→ 公证 + staple（electron-builder 内置 notarize）。

### 1. electron-builder.yml

```yaml
mac:
  target: dmg
  icon: assets/icon.png
  category: public.app-category.productivity
  # 真签名：identity 留空 → electron-builder 从钥匙串自动发现 Developer ID Application
  # （CI 下由 CSC_LINK 导入临时 keychain；本地由 Xcode 管理）。identity: null / "-" 都会
  # 破坏真签名或退回 ad-hoc，必须删掉。
  hardenedRuntime: true
  entitlements: build/entitlements.mac.plist
  entitlementsInherit: build/entitlements.mac.inherit.plist
  notarize: true
  extendInfo:
    NSMicrophoneUsageDescription: AIC Desktop 需要麦克风用于语音交互（桌宠/聊天语音输入）
```

### 2. entitlements（新增 `desktop/build/entitlements.mac.plist`）

Electron 公证必备三件套 + 网络：

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>com.apple.security.cs.allow-jit</key>
  <true/>
  <key>com.apple.security.cs.allow-unsigned-executable-memory</key>
  <true/>
  <key>com.apple.security.cs.disable-library-validation</key>
  <true/>
  <key>com.apple.security.network.client</key>
  <true/>
  <key>com.apple.security.network.server</key>
  <true/>
</dict>
</plist>
```

- 麦克风/摄像头走 TCC（用户在系统设置授权），不靠 entitlement；`NSMicrophoneUsageDescription` 已在 `extendInfo`。
- `entitlements.mac.inherit.plist` 给子进程（Electron helper）：只需上面三件套。

### 3. afterPack 钩子：ad-hoc 与真签名二选一

`scripts/after-pack-adhoc-sign.js`（或改名 `after-pack-sign.js`，改名要同轮改 electron-builder.yml 与 CHANGELOG）：

- 走真签名路径（`CSC_LINK` 存在或钥匙串里有 Developer ID 证书）→ **不做 ad-hoc 深签**。深签会覆盖真签名并直接使公证失败。
- 无证书（本地开发 / CI 未配 Secrets）→ 保留现有 ad-hoc 深签，保证仍能出包。
- 建议用显式开关 `AIC_DESKTOP_SIGN=identity|adhoc`（默认 `adhoc`），CI 签名构建时设 `identity`；避免"环境里恰好有证书"就改变签名路径。

### 4. afterSign 钩子（新增 `scripts/after-sign-nested.mjs`）

osx-sign 只签 app 本体、Frameworks、helper，**不签 `Contents/Resources/` 下的裸 Mach-O**；notary 对未签名的嵌套代码直接拒绝。需补签我们自己随包发的二进制（内层优先，不用 `--deep`）：

- `Contents/Resources/backend/aic-backend`
- `Contents/Resources/agent-browser/<os>-<arch>/<binary>`
- `Contents/Resources/skill-bin/**`（预编译 browser/cua 设备程序）

```sh
codesign --force --sign "<identity>" --options runtime --timestamp \
  --entitlements build/entitlements.mac.plist <file>
```

第三方已签名的嵌套 app（Google 的 Chrome for Testing.app、上游的 CuaDriver.app）**不重签**（重签会破坏它们自身的签名链）；混编 Team ID 是否被 notary 接受需实测（见风险表）。

### 5. G2 信任链（随包携带）

2025 起 Apple 的 Developer ID 改用 **G2 签发链**（issuer = `Developer ID Certification Authority / OU=G2 / O=Apple Inc.`），macOS **不预装**该中间证书与 G2 根：签名钥匙串里缺它们，codesign 就无法为签名者构建证书链（`errSecInternalComponent` + “unable to build chain to self-signed root”）。

两张公开 CA 证书随仓库带走（`desktop/build/certs/`，非机密）：

| 文件 | 主体 | 有效期 |
| --- | --- | --- |
| `DeveloperIDG2CA.cer` | Developer ID Certification Authority (G2) | 2031-09-17 |
| `AppleRootCA-G2.cer` | Apple Root CA - G2 | 2039-04-30 |

- 钩子在导入 p12 后把它们导进**同一临时钥匙串**（`after-pack-sign.js` 的 `G2_CHAIN_CERTS`）。
- CI 另把它们导进 runner 的登录钥匙串（electron-builder 自己的临时钥匙串只含 p12）。
- 旧的 `DeveloperIDCA.cer` 是 G1（2027-02-01 到期），**不适用**于本证书。

### 6. CI（`.github/workflows/build.yml`）

desktop job 已接入（仅 macOS runner）：

```yaml
      - name: Install Developer ID G2 chain (macOS)
        if: runner.os == 'Darwin'
        run: |
          for ca in desktop/build/certs/*.cer; do
            security import "$ca" -k ~/Library/Keychains/login.keychain-db
          done
          security unlock-keychain ~/Library/Keychains/login.keychain-db
      - name: Prepare notarization key (macOS)
        if: runner.os == 'Darwin'
        env:
          APPLE_API_KEY_B64: ${{ secrets.APPLE_API_KEY }}
        run: |
          if [ -z "$APPLE_API_KEY_B64" ]; then
            echo "未配置 APPLE_API_KEY：跳过公证（产物为已签名未公证）"
            exit 0
          fi
          echo "$APPLE_API_KEY_B64" | base64 --decode > "$RUNNER_TEMP/AuthKey.p8"
          chmod 600 "$RUNNER_TEMP/AuthKey.p8"
          echo "APPLE_API_KEY=$RUNNER_TEMP/AuthKey.p8" >> "$GITHUB_ENV"
      - name: Build
        env:
          CSC_LINK: ${{ secrets.CSC_LINK }}
          CSC_KEY_PASSWORD: ${{ secrets.CSC_KEY_PASSWORD }}
          APPLE_API_KEY_ID: ${{ secrets.APPLE_API_KEY_ID }}
          APPLE_API_ISSUER: ${{ secrets.APPLE_API_ISSUER }}
        run: make ${{ matrix.target }}
      - name: Verify signature (macOS)
        if: runner.os == 'Darwin'
        run: |
          APP=$(ls -d dist/mac*/AIC\ Desktop.app | head -1)
          codesign --verify --deep --strict "$APP"
          codesign -dv --verbose=2 "$APP" 2>&1 | grep -E "Authority|TeamIdentifier" || true
          xcrun stapler validate "$APP" || echo "（无公证票据：未配置公证凭据时正常）"
          spctl -a -vvv -t install "$APP" || echo "（Gatekeeper 未认可：未公证时正常）"
```

要点：

- **签名凭据导入由 electron-builder 自己完成**（`CSC_LINK` + `CSC_KEY_PASSWORD` → 临时钥匙串 + `set-key-partition-list`）；钩子另建一把钥匙串给嵌套二进制补签，两者用同一张证书。
- `CSC_KEY_PASSWORD` **必须用 ASCII**：非 ASCII 口令会让 `security set-key-partition-list -k` 报 “passphrase not correct”（钩子已容错不阻断，但 electron-builder 自身那步会失败）。
- `APPLE_API_KEY` 传 .p8 文件路径，需先落盘（上面步骤用 base64 解码）；该私钥**只能下载一次**。
- 公证是异步的（上传 + Apple 处理，通常几分钟到半小时）；失败时 electron-builder 打印 notary 日志。
- macOS 只能 macOS runner 构建（已有 macos-15 / macos-14）。
- 运行依赖由 beforePack 钩子现下（storage.googleapis.com / GitHub Releases），CI runner 直连即可，不依赖本机代理。
- 用 GitHub **Environment**（protected）限制只有 tag 构建能取到 Secrets；`CSC_LINK` 泄露等于私钥泄露，需在账户页吊销并重签。

### 7. Secrets 清单

| Secret | 内容 | 来源 |
| --- | --- | --- |
| `CSC_LINK` | Developer ID Application 证书 .p12 的 base64 | Xcode 导出或 `security export` |
| `CSC_KEY_PASSWORD` | p12 导出密码 | 自定 |
| `CSC_NAME`（可选） | 证书全名 `Developer ID Application: Beijing Qiongju Technology Co., Ltd. (<TEAMID>)` | 账户页 |
| `APPLE_API_KEY` | App Store Connect API 私钥 .p8（路径或 base64） | App Store Connect → Users and Access → Integrations → App Store Connect API |
| `APPLE_API_KEY_ID` | Key ID | 同上 |
| `APPLE_API_ISSUER` | Issuer ID（团队页顶部 UUID） | 同上 |

## 前置（人工，Apple 侧）

1. 会籍激活：2026-11-05 已付款（$98.99/年，Registration ID 6273V7XDND），等待激活（可能 24–48h）。
2. 签发 **Developer ID Application** 证书：Xcode → Settings → Accounts → Manage Certificates → `+`。
3. App Store Connect 接受协议 → Users and Access → Integrations → 建 App Store Connect API Key（角色 Developer）。

## 风险与退路

| 风险 | 表现 | 退路 |
| --- | --- | --- |
| notary 拒绝混编 Team ID 的嵌套第三方 app | 公证报 "not signed" / "signature is invalid" | 实测为准；若被拒：① 只重签自己发的二进制；② 把 Chrome 改为运行时下载（破坏"运行时无需下载"设计，最后手段）；③ 把第三方 app 移出 app bundle，放 dmg 内同级目录 |
| 随包裸 Mach-O 未签 | notary 报 "The executable ... is not signed" | afterSign 钩子补签（见上） |
| hardened runtime 缺 entitlement | 运行即崩（Electron JIT） | entitlements 三件套 + `entitlementsInherit` |
| 公证超时 / 失败 | 构建失败 | 保留未公证产物；重试；本地复现 |
| 私钥外泄 | 证书被盗用 | p12 只进 Secrets；Environment 保护；限定 tag 构建；定期吊销重签 |
| ad-hoc 深签与真签名冲突 | 真签名被覆盖，公证失败 | afterPack 钩子按开关二选一，绝不同时执行 |
| 证书主体名只能是英文 | macOS 系统提示显示英文开发者名 | 无解（Apple 只写拉丁字母）；App Store 的中文显示名走 Seller Name，不受此限 |

## 执行顺序

- **Phase 0（人工）**：激活会籍 → 签发 Developer ID Application 证书 → 建 API Key。
- **Phase 1（本地，mbp）**：改 electron-builder.yml + 新增 entitlements + afterPack 判定 + afterSign 补签 → 出签名 `.app`，`codesign --verify --deep --strict` 与 `spctl -a -vvv` 通过。
- **Phase 2（本地）**：跑一次公证（API Key 方式），确认 notary 接受嵌套代码；按 Apple 的具体报错补。
- **Phase 3（CI）**：加 Secrets 与 workflow 步骤 → tag 构建产出已公证 dmg → 在一台干净 mac 上验证双击即开。
- **Phase 4（另立）**：自动更新（electron-updater + GitHub Releases 更新源 + 签名校验）。

## 验证命令

```sh
# 签名与结构
codesign -dv --verbose=4 "dist/mac-arm64/AIC Desktop.app"
codesign --verify --deep --strict "dist/mac-arm64/AIC Desktop.app"
spctl -a -vvv -t install "dist/mac-arm64/AIC Desktop.app"

# 公证票据
xcrun stapler validate "dist/mac-arm64/AIC Desktop.app"

# 嵌套二进制是否都签了
codesign -dv --verbose=2 "dist/mac-arm64/AIC Desktop.app/Contents/Resources/backend/aic-backend"

# notary 处理结果
xcrun notarytool log <submission-id> --key <p8> --key-id <id> --issuer <issuer>
```

## 未决

- **CI 首次跑通待验**：本地 mbp 的钥匙串/信任库异常（`errSecInternalComponent`，Xcode 自建证书同样失败），签名与公证改由 CI 干净 runner 执行；跑通后回填本文验收结果。
- **Team ID**：`CC6VQMAF97`（证书签发前从证书全名取得，已确认）。
- ** notary 是否接受混编 Team ID 的嵌套第三方 app**（Chrome for Testing / CuaDriver 为上游 Team）——实测确认；被拒则按上表退路处理。
- **自动更新**（electron-updater + GitHub Releases 更新源 + 签名校验）另立一轮。
- **证书主体名只能是英文**（`Beijing Qiongju Technology Co., Ltd.`），App Store 中文显示名走 Seller Name，与本方案无关。
- **p12 口令轮换**：当前口令已在前序会话与本机 JSON 备份中出现，CI 跑通后建议用随机口令重新导出并更新 Secrets。
