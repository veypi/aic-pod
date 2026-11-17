//
//  SettingsViewModel.swift
//  aic-pod · mobile/ios
//
//  设置页状态：服务地址、主题、语言、账户。
//

import Foundation
import SwiftUI

@MainActor
final class SettingsViewModel: ObservableObject {
    @Published var serverText: String
    @Published var theme: AppTheme
    @Published var language: AppLanguage
    @Published var isDeleting = false
    @Published var errorMessage: String?
    @Published var accountDeleted = false
    /// 协议地址（后台配置；未配置则不显示对应入口）。
    @Published var termsURL: String?
    @Published var privacyURL: String?

    let config: ServerConfig
    let auth: AuthManager

    init(config: ServerConfig, auth: AuthManager) {
        self.config = config
        self.auth = auth
        serverText = config.server.absoluteString
        theme = config.theme
        language = config.language
    }

    /// 已登录账号（由原生认证管理器持有）。
    var isSignedIn: Bool { auth.isSignedIn }

    /// 当前账户展示名。
    var currentUserName: String {
        auth.currentUser?.displayName ?? ""
    }

    /// 服务地址是否可用。
    var serverIsValid: Bool {
        guard let url = URL(string: normalizedServerText) else { return false }
        return serverIsValid(url)
    }

    /// 去掉输入里可能混入的空格（iOS 键盘常在冒号后补空格）。
    var normalizedServerText: String {
        serverText.replacingOccurrences(of: " ", with: "").trimmingCharacters(in: .whitespacesAndNewlines)
    }

    private func serverIsValid(_ url: URL) -> Bool {
        guard let scheme = url.scheme, scheme == "https" || scheme == "http", url.host != nil else { return false }
        return true
    }

    /// 保存服务地址（换地址即换登录档案）。
    func saveServer() {
        let trimmed = normalizedServerText
        guard let url = URL(string: trimmed), serverIsValid(url) else {
            errorMessage = config.t("settings.invalid_server")
            return
        }
        config.server = url
        serverText = url.absoluteString
        errorMessage = nil
    }

    func saveTheme(_ value: AppTheme) {
        theme = value
        config.theme = value
    }

    func saveLanguage(_ value: AppLanguage) {
        language = value
        config.language = value
    }

    /// 读取两份协议地址（用户协议 / 隐私政策）。
    func loadLegalLinks() async {
        do {
            let docs = try await LegalDocuments.fetch(server: config.server)
            termsURL = docs.first(where: { $0.kind == .terms })?.url
            privacyURL = docs.first(where: { $0.kind == .privacy })?.url
        } catch {
            termsURL = nil
            privacyURL = nil
        }
    }

    /// 退出登录：吊销服务端会话并清本机凭据。
    func signOut() async {
        await auth.signOut()
    }

    /// 注销账号：先调服务端，成功后清本机凭据。
    func deleteAccount() async {
        guard !isDeleting else { return }
        isDeleting = true
        errorMessage = nil
        defer { isDeleting = false }

        do {
            try await auth.deleteAccount()
            accountDeleted = true
        } catch {
            errorMessage = error.localizedDescription
        }
    }
}
