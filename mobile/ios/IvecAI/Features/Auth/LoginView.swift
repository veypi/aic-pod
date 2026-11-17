//
//  LoginView.swift
//  aic-pod · mobile/ios
//
//  原生登录 / 注册页。逻辑对齐 vbase 的登录注册页：
//  密码登录 / 验证码登录切换、忘记密码、注册（按后台要求要邮箱或手机号 + 验证码）、
//  协议同意、服务器地址切换。
//
//  登录成功后把会话 cookie 注入 webview，网页即认这个登录态。
//

import SwiftUI

struct LoginView: View {
    @ObservedObject var config: ServerConfig
    /// 登录成功：回调会话 cookie。
    var onLogin: ([HTTPCookie]) -> Void
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

    var body: some View {
        ZStack {
            Color.brandBackground.ignoresSafeArea()

            ScrollView {
                VStack(spacing: 22) {
                    topBar
                    brand
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
            if useCode && codeTarget.isEmpty { codeTarget = username }
        }
    }

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
            segment(config.t("login.password_login"), active: !useCode) { useCode = false }
            segment(config.t("login.code_login"), active: useCode) { useCode = true }
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
                field(config.t("login.username"), text: $username)
                field(config.t("login.password"), text: $password, secure: true)
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
                field(config.t("login.account_placeholder"), text: $username)
                field(config.t("login.password"), text: $password, secure: true)
            }
        }
    }

    /// 目标 + 验证码一行（发送按钮在右）。
    private func codeRow(type: String?, target: Binding<String>, code: Binding<String>, purpose: String) -> some View {
        VStack(spacing: 10) {
            let placeholder = type == "email" ? config.t("login.email_placeholder")
                : (type == "sms" ? config.t("login.phone_placeholder") : config.t("login.account_placeholder"))
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
            field(config.t("login.code_placeholder"), text: code, keyboard: .numberPad)
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
                Text(config.t("login.forgot_password"))
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
            text = text + Text(document.title)
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
                errorMessage = error.localizedDescription
            }
        }
    }

    private func submit() {
        guard !isBusy else { return }
        isBusy = true
        errorMessage = nil
        Task { @MainActor in
            defer { isBusy = false }
            let client = APIClient(server: config.server)
            do {
                let cookies: [HTTPCookie]
                if isRegister {
                    cookies = try await client.register(
                        username: username,
                        password: password,
                        email: authConfig.requireEmail ? email : nil,
                        phone: authConfig.requirePhone ? phone : nil,
                        emailCode: authConfig.requireEmail ? emailCode : nil,
                        phoneCode: authConfig.requirePhone ? phoneCode : nil
                    )
                } else if useCode {
                    let type = codeTarget.contains("@") ? "email" : "sms"
                    cookies = try await client.loginWithCode(type: type, target: codeTarget, code: code)
                } else {
                    cookies = try await client.login(username: username, password: password)
                }
                if let token = APIClient.token(in: cookies) {
                    KeychainStore.set(token, for: KeychainStore.Account.authToken)
                }
                KeychainStore.set(Self.serialize(cookies), for: KeychainStore.Account.sessionCookies)
                password = ""
                confirmPassword = ""
                onLogin(cookies)
            } catch {
                errorMessage = error.localizedDescription
            }
        }
    }

    /// 把 cookie 序列化成可存进 Keychain 的字符串。
    static func serialize(_ cookies: [HTTPCookie]) -> String {
        cookies
            .map { "\($0.name)=\($0.value); Domain=\($0.domain); Path=\($0.path)" }
            .joined(separator: "\n")
    }
}
