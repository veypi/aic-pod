//
//  LoginView.swift
//  aic-pod · mobile/ios
//
//  原生登录 / 注册页。逻辑对齐 vbase 的登录注册页：
//  密码登录 / 验证码登录切换、忘记密码、注册（按后台要求要邮箱或手机号 + 验证码）、
//  协议同意、服务器地址切换。
//
//  登录成功后凭据由原生认证管理器持有（Keychain），网页不再拿令牌。
//

import SwiftUI

struct LoginView: View {
    @ObservedObject var config: ServerConfig
    @ObservedObject var auth: AuthManager
    /// 打开设置页。
    var onOpenSettings: () -> Void

    @State private var isRegister = false
    @State private var useCode = false
    @State private var username = ""
    @State private var password = ""
    @State private var confirmPassword = ""
    @State private var email = ""
    @State private var emailCode = ""
    @State private var phone = ""
    @State private var phoneCode = ""
    @State private var codeTarget = ""
    @State private var code = ""
    @State private var serverText = ""
    @State private var showServerField = false
    @State private var isBusy = false
    @State private var errorMessage: String?
    @State private var legalAgreed = false
    @State private var legalDocs: [LegalDocument] = []
    @State private var authConfig = AuthConfig()
    @State private var countdown = 0
    /// 已启用的三方登录提供商。
    @State private var providers: [AuthProvider] = []
    /// 待打开的三方授权页。
    @State private var oauthTarget: OAuthTarget?

    var body: some View {
        ZStack {
            Color.brandBackground.ignoresSafeArea()

            ScrollView {
                VStack(spacing: 22) {
                    topBar
                    brand
                    if !providers.isEmpty {
                        social
                    }
                    if authConfig.codeLoginEnabled && !isRegister {
                        loginTypeSwitch
                    }
                    fields
                    if !isRegister {
                        forgotLink
                    }
                    if !legalDocs.isEmpty {
                        consent
                    }
                    if let error = errorMessage {
                        Text(error)
                            .font(.system(size: 13))
                            .foregroundStyle(Color.red)
                            .frame(maxWidth: .infinity, alignment: .leading)
                    }
                    primaryButton
                    switchLink
                }
                .padding(.horizontal, 28)
                .padding(.bottom, 40)
            }
        }
        .task {
            serverText = config.server.absoluteString
            legalDocs = (try? await LegalDocuments.fetch(server: config.server)) ?? []
            authConfig = (try? await APIClient(server: config.server).fetchAuthConfig()) ?? AuthConfig()
            providers = (try? await APIClient(server: config.server).fetchProviders()) ?? []
            if useCode && codeTarget.isEmpty { codeTarget = username }
        }
        .sheet(item: $oauthTarget) { target in
            OAuthWebSheet(
                target: target,
                platformHost: config.server.host,
                cancelTitle: config.t("settings.cancel"),
                onCookies: { cookies in
                    do {
                        try auth.adopt(cookies: cookies)
                    } catch {
                        errorMessage = config.errorText(error)
                    }
                },
                onError: { message in
                    errorMessage = message
                }
            )
        }
    }

    // MARK: - 三方登录

    private var social: some View {
        HStack(spacing: 10) {
            ForEach(providers) { provider in
                Button {
                    startOAuth(provider)
                } label: {
                    providerBadge(provider)
                }
                .buttonStyle(.plain)
                .disabled(isBusy)
                .accessibilityLabel(provider.name)
            }
        }
        .padding(.bottom, 2)
    }

    /// 三方按钮：本地图标优先（微信 / 钉钉 / 飞书），其次 Font Awesome 品牌字形，
    /// 都没有才用首字母。
    private func providerBadge(_ provider: AuthProvider) -> some View {
        let keys = [provider.icon.lowercased(), provider.code.lowercased()]
        let asset = keys.compactMap { Self.localProviderAssets[$0] }.first
        let glyph = keys.compactMap { Self.brandGlyphs[$0] }.first
        return ZStack {
            Circle()
                .stroke(Color.black.opacity(0.12), lineWidth: 1)
            if let asset {
                Image(asset)
                    .resizable()
                    .scaledToFit()
                    .frame(width: 20, height: 20)
            } else if let glyph {
                Text(glyph)
                    .font(.custom("Font Awesome 6 Brands", size: 18))
                    .foregroundStyle(Color.brandPrimary)
            } else {
                Text(String(provider.name.prefix(1)).uppercased())
                    .font(.system(size: 15, weight: .semibold))
                    .foregroundStyle(Color.brandPrimary)
            }
        }
        .frame(width: 44, height: 44)
        .contentShape(Circle())
    }

    private func startOAuth(_ provider: AuthProvider) {
        guard !isBusy else { return }
        errorMessage = nil
        Task { @MainActor in
            do {
                let url = try await auth.authorizeThirdParty(provider: provider.code)
                oauthTarget = OAuthTarget(url: url)
            } catch {
                errorMessage = config.errorText(error)
            }
        }
    }

    /// FA 没有的品牌用本地矢量图标（与 vbase 网页登录页同一份来源）。
    private static let localProviderAssets: [String: String] = [
        "wechat": "ProviderWechat", "weixin": "ProviderWechat",
        "dingtalk": "ProviderDingtalk", "feishu": "ProviderFeishu",
    ]

    /// Font Awesome 6 Brands 字形（码点在 PUA 区）。
    private static let brandGlyphs: [String: String] = [
        "google": "\u{f1a0}", "github": "\u{f09b}", "weixin": "\u{f1d7}", "wechat": "\u{f1d7}",
        "apple": "\u{f179}", "microsoft": "\u{f3ca}", "twitter": "\u{f099}", "x-twitter": "\u{e61b}",
        "discord": "\u{f392}", "qq": "\u{f1d6}", "weibo": "\u{f18a}", "alipay": "\u{f642}",
        "gitlab": "\u{f296}", "facebook": "\u{f39e}", "linkedin": "\u{f0e1}", "amazon": "\u{f270}",
        "bitbucket": "\u{f171}", "steam": "\u{f1b6}", "twitch": "\u{f1e8}", "reddit": "\u{f1a1}",
        "git": "\u{f1d3}", "android": "\u{f17b}", "linux": "\u{f17c}", "windows": "\u{f17a}",
    ]

    // MARK: - 顶部

    private var topBar: some View {
        HStack {
            Spacer()
            Button {
                onOpenSettings()
            } label: {
                Image(systemName: "gearshape")
                    .font(.system(size: 15, weight: .regular))
                    .foregroundStyle(Color.black.opacity(0.35))
            }
            .buttonStyle(.plain)
            .accessibilityLabel(config.t("settings.title"))
        }
        .padding(.top, 6)
    }

    private var brand: some View {
        VStack(spacing: 10) {
            Image("BrandLogo")
                .resizable()
                .frame(width: 64, height: 64)
                .cornerRadius(16)
            Text(AppConfig.displayName)
                .font(.system(size: 22, weight: .semibold))
                .foregroundStyle(Color.brandPrimary)
            Text(config.t("login.subtitle"))
                .font(.system(size: 13))
                .foregroundStyle(Color.brandSecondary)
        }
        .padding(.top, 4)
    }

    private var loginTypeSwitch: some View {
        HStack(spacing: 0) {
            segment(config.t("login.password"), active: !useCode) { useCode = false }
            segment(config.t("login.code"), active: useCode) { useCode = true }
        }
        .padding(3)
        .background(Color.black.opacity(0.05))
        .clipShape(RoundedRectangle(cornerRadius: 9, style: .continuous))
    }

    private func segment(_ title: String, active: Bool, action: @escaping () -> Void) -> some View {
        Button {
            action()
        } label: {
            Text(title)
                .font(.system(size: 13, weight: active ? .semibold : .regular))
                .foregroundStyle(active ? Color.brandPrimary : Color.brandSecondary)
                .frame(maxWidth: .infinity)
                .padding(.vertical, 7)
                .background(active ? Color.white : Color.clear)
                .clipShape(RoundedRectangle(cornerRadius: 7, style: .continuous))
        }
        .buttonStyle(.plain)
    }

    // MARK: - 表单

    @ViewBuilder
    private var fields: some View {
        VStack(spacing: 10) {
            if isRegister {
                field(config.t("login.username_label"), text: $username)
                field(config.t("login.password_value"), text: $password, secure: true)
                field(config.t("login.confirm_password"), text: $confirmPassword, secure: true)
                if authConfig.requireEmail {
                    codeRow(type: "email", target: $email, code: $emailCode, purpose: "register")
                }
                if authConfig.requirePhone {
                    codeRow(type: "sms", target: $phone, code: $phoneCode, purpose: "register")
                }
            } else if useCode {
                codeRow(type: nil, target: $codeTarget, code: $code, purpose: "login")
            } else {
                field(config.t("login.placeholder_account"), text: $username)
                field(config.t("login.password_value"), text: $password, secure: true)
            }
        }
    }

    /// 目标 + 验证码一行（发送按钮在右）。
    private func codeRow(type: String?, target: Binding<String>, code: Binding<String>, purpose: String) -> some View {
        VStack(spacing: 10) {
            let placeholder = type == "email" ? config.t("login.placeholder_email")
                : (type == "sms" ? config.t("login.placeholder_phone") : config.t("login.placeholder_account"))
            HStack(spacing: 8) {
                field(placeholder, text: target, keyboard: type == "email" ? .emailAddress : .default)
                Button {
                    let kind = type ?? (target.wrappedValue.contains("@") ? "email" : "sms")
                    sendCode(type: kind, target: target.wrappedValue, purpose: purpose)
                } label: {
                    Text(countdown > 0 ? "\(countdown)s" : config.t("login.send_code"))
                        .font(.system(size: 13))
                        .foregroundStyle(countdown > 0 ? Color.brandSecondary : Color.brandPrimary)
                        .frame(width: 92, height: 46)
                        .background(Color.white)
                        .overlay(RoundedRectangle(cornerRadius: 10).stroke(Color.black.opacity(0.10)))
                }
                .buttonStyle(.plain)
                .disabled(countdown > 0)
            }
            field(config.t("login.placeholder_code"), text: code, keyboard: .numberPad)
        }
    }

    private func field(_ placeholder: String, text: Binding<String>, secure: Bool = false,
                       keyboard: UIKeyboardType = .default) -> some View {
        Group {
            if secure {
                SecureField(placeholder, text: text)
            } else {
                TextField(placeholder, text: text)
            }
        }
        .textFieldStyle(.plain)
        .font(.system(size: 15))
        .padding(.horizontal, 13)
        .frame(height: 46)
        .background(Color.white)
        .clipShape(RoundedRectangle(cornerRadius: 10, style: .continuous))
        .overlay(RoundedRectangle(cornerRadius: 10, style: .continuous).stroke(Color.black.opacity(0.10)))
        .autocorrectionDisabled()
        .textInputAutocapitalization(.never)
        .keyboardType(keyboard)
    }

    private var forgotLink: some View {
        HStack {
            Spacer()
            Button {
                UIApplication.shared.open(config.server.sitePath("/vb/forgot-password"))
            } label: {
                Text(config.t("login.forgot"))
                    .font(.system(size: 13))
                    .foregroundStyle(Color.brandSecondary)
            }
            .buttonStyle(.plain)
        }
    }

    private var consent: some View {
        HStack(alignment: .top, spacing: 8) {
            Image(systemName: legalAgreed ? "checkmark.square.fill" : "square")
                .font(.system(size: 15))
                .foregroundStyle(legalAgreed ? Color.brandPrimary : Color.black.opacity(0.35))
                .onTapGesture { legalAgreed.toggle() }
            consentText
                .font(.system(size: 12))
                .foregroundStyle(Color.brandSecondary)
                .fixedSize(horizontal: false, vertical: true)
        }
    }

    private var consentText: Text {
        var text = Text(config.t("consent.agree_prefix"))
        for document in legalDocs {
            text = text + Text(config.t(document.titleKey))
                .foregroundStyle(Color.brandPrimary)
                .underline(true)
            text = text + Text(document.kind == .terms ? config.t("consent.and") : "")
        }
        return text
    }

    private var primaryButton: some View {
        Button {
            submit()
        } label: {
            Text(isRegister ? config.t("login.register") : config.t("login.submit"))
                .font(.system(size: 16, weight: .semibold))
                .foregroundStyle(Color.white)
                .frame(maxWidth: .infinity)
                .frame(height: 48)
                .background(canSubmit ? Brand.ink : Color.black.opacity(0.25))
                .clipShape(RoundedRectangle(cornerRadius: 10, style: .continuous))
        }
        .buttonStyle(.plain)
        .disabled(!canSubmit || isBusy)
    }

    private var switchLink: some View {
        Button {
            isRegister.toggle()
            useCode = false
            errorMessage = nil
        } label: {
            Text(isRegister ? config.t("login.have_account") : config.t("login.no_account"))
                .font(.system(size: 13))
                .foregroundStyle(Color.brandSecondary)
        }
        .buttonStyle(.plain)
    }

    // MARK: - 逻辑

    private var canSubmit: Bool {
        if isRegister {
            guard !username.isEmpty, !password.isEmpty, password == confirmPassword else { return false }
            if authConfig.requireEmail && (email.isEmpty || emailCode.isEmpty) { return false }
            if authConfig.requirePhone && (phone.isEmpty || phoneCode.isEmpty) { return false }
        } else if useCode {
            guard !codeTarget.isEmpty, !code.isEmpty else { return false }
        } else {
            guard !username.isEmpty, !password.isEmpty else { return false }
        }
        if !legalDocs.isEmpty && !legalAgreed { return false }
        return true
    }

    private func sendCode(type: String, target: String, purpose: String) {
        guard !target.isEmpty else {
            errorMessage = config.t("login.target_required")
            return
        }
        Task { @MainActor in
            do {
                try await APIClient(server: config.server).sendVerificationCode(type: type, target: target, purpose: purpose)
                countdown = 60
                while countdown > 0 {
                    try? await Task.sleep(for: .seconds(1))
                    countdown -= 1
                }
            } catch {
                errorMessage = config.errorText(error)
            }
        }
    }

    private func submit() {
        guard !isBusy else { return }
        isBusy = true
        errorMessage = nil
        Task { @MainActor in
            defer { isBusy = false }
            do {
                if isRegister {
                    var body: [String: String] = ["username": username, "password": password]
                    if authConfig.requireEmail {
                        body["email"] = email
                        body["email_code"] = emailCode
                    }
                    if authConfig.requirePhone {
                        body["phone"] = phone
                        body["phone_code"] = phoneCode
                    }
                    try await auth.register(body)
                } else if useCode {
                    let type = codeTarget.contains("@") ? "email" : "sms"
                    try await auth.loginCode(type: type, target: codeTarget, code: code)
                } else {
                    try await auth.login(username: username, password: password)
                }
                password = ""
                confirmPassword = ""
            } catch {
                errorMessage = config.errorText(error)
            }
        }
    }
}
