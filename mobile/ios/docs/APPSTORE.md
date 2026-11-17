# APP STORE 上架清单（ivec ai · iOS）

> 状态同步：2026-11-17。勾选项按「谁能做」分组。

## 一、壳侧代码（已完成部分）

- [x] Xcode 工程（`scripts/gen-project.py` 生成，可重复执行）
- [x] 工作区（WKWebView 加载 `https://ivec-ai.com/u`）
- [x] 设置页（原生分组列表：服务 / 外观 / 账户 / 隐私 / 关于）
- [x] 首启协议同意弹层（清单取自 `auth.legal_documents`）
- [x] 注销账户入口与确认层（调 `DELETE /vb/api/auth/account`）
- [x] Keychain 存凭据、UserDefaults 存本地偏好
- [x] `Info.plist`（权限最小化、`ITSAppUsesNonExemptEncryption=false`）
- [x] `PrivacyInfo.xcprivacy`（required reason `CA92.1`）
- [x] AppIcon（`mobile/brand/ivec-app-icon-1024.png`）

## 二、Apple 后台（需账号持有人操作）

- [ ] **Bundle ID** `com.ivec.ai` 在 Apple Developer 门户创建
- [ ] **正式 Provisioning Profile**（App Store distribution）
- [ ] **App Store Connect 建 App 记录**，占名 `ivec ai`
- [ ] **主体信息更正**：地址 `No.1 Yunshan Road / 101100`、统一社会信用代码 `91110114MAK7U7DK9L`
      （当前 D&B 仍是旧值 91113350，会卡卖家信息核对）
- [ ] **App 隐私问卷**（对应 `PrivacyInfo.xcprivacy` 与实际行为）
- [ ] **出口合规**：仅标准加密 → 走豁免
- [ ] **中国区备案号**：填入 App Information（三端备案已下号）
- [ ] **演示账号**（供审核员登录体验）

## 三、素材

- [ ] 截图：6.9" 与 6.5"（真机或模拟器截图，展示工作区与设置）
- [ ] 描述、关键词、分类（工具 / 效率）、年龄分级
- [ ] 隐私政策 URL：`https://ivec-ai.com/legal/privacy`
- [ ] 用户协议 URL：`https://ivec-ai.com/legal/terms`

## 四、审核风险与对策

| 风险 | 对策 | 状态 |
|---|---|---|
| 4.2 最低功能（纯 webview 壳） | 原生设置页 / 首启同意弹层 / 注销账户 / 文件导入分享 | 原生设置、同意、注销已具备；文件导入分享待补 |
| 4.7 迷你应用 / 只是网站包装 | 壳提供独立原生能力，网页只是工作区 | 需在提审说明里写清楚 |
| 5.1.1(v) 账号删除 | APP 内「设置 → 账户 → 注销账户」 | 已实现（依赖后端接口） |
| 中国区备案 | 备案号回填 App Store Connect | 待回填 |
| 首启隐私同意 | 原生弹层，未同意不进入工作区 | 已实现 |

## 五、依赖的后端改动

- [ ] **vbase 新增 `DELETE /vb/api/auth/account`**（自助注销账号）。
      目前只有 `POST /vb/api/auth/logout`（登出），不满足 5.1.1(v)。
      口径建议：吊销全部会话与凭据 → 删除或匿名化账号数据 → 返回成功。
      aic 侧的业务数据（会话、消息、文件、生成产物）需要一并清理或标记为删除。
- [ ] **协议清单可达性**：`GET /vb/api/config` 对匿名返回 `auth.legal_documents`（已具备）。

## 六、发布前检查

- [ ] `python3 scripts/gen-project.py` 后工程可被 Xcode 打开
- [ ] `bash scripts/build.sh run` 在模拟器跑通：同意 → 工作区 → 设置 → 注销
- [ ] 深色模式、动态字体、横竖屏、无网络时的降级
- [ ] 仓库不含任何私钥 / 证书 / Profile
