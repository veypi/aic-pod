//
//  AppConfig.swift
//  aic-pod · mobile/ios
//
//  壳侧的构建常量与默认值。服务端地址与协议清单是运行时可改的，
//  这里只放不可变的那部分。
//

import Foundation

enum AppConfig {
    /// 应用名（商店名 = 备案名 = 软著名的锚），提交后不可改。
    /// 这只用于系统层面（主屏名称、Info.plist），App 内部展示用 displayName。
    static let appName = "ivec AI"

    /// App 内部展示用的品牌标记（与官网/网页一致：ivec.ai）。
    static let displayName = "ivec.ai"

    /// 包名（与 Android / 鸿蒙一致）。
    static let bundleID = "com.ivec.ai"

    /// 默认服务域名（已 ICP 备案，APP 备案用同一域名）。
    static let defaultServer = URL(string: "https://ivec-ai.com")!

    /// 工作区默认路由：平台首页（登录后落到 Agent 会话）。
    /// 注意：URL.appending(path:) 会把斜杠编码成 %2F，这里必须是普通字符串拼接。
    static let workspacePath = "/"

    /// 最低系统版本（2026-11-17 定）。
    static let minimumOSVersion = "17.0"

    /// 协议地址配置键（vbase vb scope，公开配置）：用户协议与隐私政策各一个。
    static let legalTermsKey = "auth.legal.terms_url"
    static let legalPrivacyKey = "auth.legal.privacy_url"

    /// 注销账号接口（vbase）：DELETE /api/auth/account，需要 Bearer 令牌。
    static let accountDeletePath = "/vb/api/auth/account"

    /// 公共配置接口：登录页与壳都从这里读协议清单。
    static let configPath = "/vb/api/config"
}
