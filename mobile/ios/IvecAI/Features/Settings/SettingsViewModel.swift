//
//  SettingsViewModel.swift
//  aic-pod · mobile/ios
//
//  设置页状态：服务地址、主题、语言、账户注销。
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

    init(config: ServerConfig) {
        self.config = config
        serverText = config.server.absoluteString
        theme = config.theme
        language = config.language
    }

    /// 已登录账号（来自 Keychain 里是否存在令牌）。
    var isSignedIn: Bool {
        guard let token = KeychainStore.get(KeychainStore.Account.authToken) else { return false }
        return !token.isEmpty
    }

    /// 服务地址是否可用。
    var serverIsValid: Bool {
        guard let url = URL(string: normalizedServerText), serverIsValid(url) else { return false }
        return true
    }

    /// 去掉输入里可能混入的空格（iOS 键盘常在冒号后补空格）。
    var normalizedServerText: String {
        serverText.replacingOccurrences(of: " ", with: "").trimmingCharacters(in: .whitespacesAndNewlines)
    }

    private func serverIsValid(_ url: URL) -> Bool {
        guard let scheme = url.scheme, scheme == "https" || scheme == "http", url.host != nil else { return false }
        return true
    }

    /// 保存服务地址。
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

    /// 注销账号：先调服务端，成功后清本机凭据。
    func deleteAccount() async {
        guard !isDeleting else { return }
        isDeleting = true
        errorMessage = nil
        defer { isDeleting = false }

        let client = APIClient(server: config.server)
        do {
            try await client.deleteAccount()
            KeychainStore.remove(KeychainStore.Account.authToken)
            accountDeleted = true
        } catch {
            errorMessage = error.localizedDescription
        }
    }
}
