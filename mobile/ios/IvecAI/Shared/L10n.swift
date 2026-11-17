//
//  L10n.swift
//  aic-pod · mobile/ios
//
//  壳侧文案（中英）。原生壳暂不引入完整资源目录，先用集中字典；
//  需要扩第三种语言时再迁到 String Catalog。
//

import Foundation

enum L10n {
    static let table: [AppLanguage: [String: String]] = [
        .zh: [
            "settings.title": "设置",
            "settings.server": "服务",
            "settings.server_address": "服务器地址",
            "settings.save_reconnect": "保存并重新连接",
            "settings.connection": "连接状态",
            "settings.connected": "已连接",
            "settings.not_signed_in": "未登录",
            "settings.appearance": "外观",
            "settings.theme": "主题",
            "settings.language": "界面语言",
            "settings.follow_system": "跟随系统",
            "settings.light": "浅色",
            "settings.dark": "深色",
            "settings.chinese": "中文",
            "settings.english": "English",
            "settings.account": "账户",
            "settings.signed_in": "已登录",
            "settings.delete_account": "注销账户",
            "settings.delete_account_note": "注销会清除本机令牌与凭据，并按隐私政策处理账号数据，不可恢复。",
            "settings.privacy": "隐私",
            "settings.privacy_policy": "隐私政策",
            "settings.terms": "用户协议",
            "settings.about": "关于",
            "settings.version": "版本",
            "settings.license": "许可证",
            "settings.open_source": "开源地址",
            "settings.cancel": "取消",
            "settings.invalid_server": "服务地址无效。",
            "settings.operation_failed": "操作失败",

            "login.username": "用户名",
            "login.password": "密码",
            "login.confirm_password": "确认密码",
            "login.submit": "登录",
            "login.register": "注册",
            "login.have_account": "已有账号，去登录",
            "login.no_account": "没有账号，去注册",
            "login.code_login": "验证码登录",
            "login.password_login": "密码登录",
            "login.verification_code": "验证码",
            "login.send_code": "获取验证码",
            "login.forgot_password": "忘记密码？",
            "login.email": "邮箱",
            "login.phone": "手机号",
            "login.subtitle": "登录以继续",
            "login.email_placeholder": "邮箱",
            "login.phone_placeholder": "手机号",
            "login.code_placeholder": "验证码",
            "login.account_placeholder": "用户名或邮箱",

            "workspace.session": "会话",
            "workspace.skills": "技能",
            "workspace.files": "文件",
            "workspace.settings": "设置",

            "consent.title": "欢迎使用 %@",
            "consent.body": "开始使用前，请阅读并同意用户协议与隐私政策。我们按最小必要原则收集数据，不读取通讯录、位置与设备标识符。",
            "consent.agree": "同意并继续",
            "consent.decline": "暂不同意",
            "consent.decline_notice": "需要同意协议后才能使用 %@。",
            "consent.load_failed": "协议清单读取失败，可稍后在设置中查看。",
            "consent.agree_prefix": "我已阅读并同意",
            "consent.and": "与",

            "delete.title": "注销账户？",
            "delete.message": "注销后本机令牌与凭据将被清除，账号数据按隐私政策处理，该操作不可恢复。"
        ],
        .en: [
            "settings.title": "Settings",
            "settings.server": "Server",
            "settings.server_address": "Server Address",
            "settings.save_reconnect": "Save & Reconnect",
            "settings.connection": "Connection",
            "settings.connected": "Connected",
            "settings.not_signed_in": "Not Signed In",
            "settings.appearance": "Appearance",
            "settings.theme": "Theme",
            "settings.language": "Language",
            "settings.follow_system": "Follow System",
            "settings.light": "Light",
            "settings.dark": "Dark",
            "settings.chinese": "中文",
            "settings.english": "English",
            "settings.account": "Account",
            "settings.signed_in": "Signed In",
            "settings.delete_account": "Delete Account",
            "settings.delete_account_note": "Deleting removes the token and credentials on this device; account data is handled per the Privacy Policy. This cannot be undone.",
            "settings.privacy": "Privacy",
            "settings.privacy_policy": "Privacy Policy",
            "settings.terms": "Terms of Service",
            "settings.about": "About",
            "settings.version": "Version",
            "settings.license": "License",
            "settings.open_source": "Open Source",
            "settings.cancel": "Cancel",
            "settings.invalid_server": "Invalid server address.",
            "settings.operation_failed": "Operation Failed",

            "login.username": "Username",
            "login.password": "Password",
            "login.confirm_password": "Confirm Password",
            "login.submit": "Sign In",
            "login.register": "Create Account",
            "login.have_account": "Already have an account? Sign in",
            "login.no_account": "No account? Create one",
            "login.code_login": "Code",
            "login.password_login": "Password",
            "login.verification_code": "Verification code",
            "login.send_code": "Send code",
            "login.forgot_password": "Forgot password?",
            "login.email": "Email",
            "login.phone": "Mobile",
            "login.subtitle": "Sign in to continue",
            "login.email_placeholder": "Email",
            "login.phone_placeholder": "Mobile number",
            "login.code_placeholder": "Code",
            "login.account_placeholder": "Username or email",

            "workspace.session": "Chats",
            "workspace.skills": "Skills",
            "workspace.files": "Files",
            "workspace.settings": "Settings",

            "consent.title": "Welcome to %@",
            "consent.body": "Before you start, please read and agree to the Terms of Service and the Privacy Policy. We collect only what the service needs, and never read your contacts, location or device identifiers.",
            "consent.agree": "Agree & Continue",
            "consent.decline": "Not Now",
            "consent.decline_notice": "You need to agree to the terms before using %@.",
            "consent.load_failed": "Could not load the agreement list. You can check it later in Settings.",
            "consent.agree_prefix": "I have read and agree to",
            "consent.and": "and",

            "delete.title": "Delete Account?",
            "delete.message": "The token and credentials on this device will be removed, account data is handled per the Privacy Policy. This cannot be undone."
        ]
    ]

    /// 取文案；缺 key 时回落到中文，再缺就返回 key 本身。
    static func text(_ key: String, language: AppLanguage) -> String {
        if let value = table[language]?[key] { return value }
        if let value = table[.zh]?[key] { return value }
        return key
    }

    /// 带一个占位符的文案（%@）。
    static func text(_ key: String, language: AppLanguage, _ argument: String) -> String {
        text(key, language: language)
            .replacingOccurrences(of: "%@", with: argument)
    }
}
