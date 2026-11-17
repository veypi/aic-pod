//
//  KeychainStore.swift
//  aic-pod · mobile/ios
//
//  凭据存储。网页登录后的令牌由页面通过 ShellBridge 交给壳，
//  壳存进 Keychain（不落 UserDefaults、不进日志）。
//

import Foundation
import Security

enum KeychainStore {
    private static let service = "ai.ivec.desktop"

    /// 保存字符串凭据。
    static func set(_ value: String, for account: String) {
        let query: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
            kSecAttrAccount as String: account
        ]
        SecItemDelete(query as CFDictionary)

        var attributes = query
        attributes[kSecValueData as String] = Data(value.utf8)
        attributes[kSecAttrAccessible as String] = kSecAttrAccessibleAfterFirstUnlock
        SecItemAdd(attributes as CFDictionary, nil)
    }

    /// 读取字符串凭据。
    static func get(_ account: String) -> String? {
        let query: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
            kSecAttrAccount as String: account,
            kSecReturnData as String: true,
            kSecMatchLimit as String: kSecMatchLimitOne
        ]
        var item: CFTypeRef?
        guard SecItemCopyMatching(query as CFDictionary, &item) == errSecSuccess,
              let data = item as? Data else { return nil }
        return String(data: data, encoding: .utf8)
    }

    /// 删除凭据。
    static func remove(_ account: String) {
        let query: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
            kSecAttrAccount as String: account
        ]
        SecItemDelete(query as CFDictionary)
    }

    enum Account {
        /// 网页登录后的 auth 令牌。
        static let authToken = "auth.token"
        /// 会话 cookie 串（冷启动恢复登录态用）。
        static let sessionCookies = "auth.cookies"
    }
}
