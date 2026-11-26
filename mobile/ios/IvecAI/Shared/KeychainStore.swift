//
//  KeychainStore.swift
//  aic-pod · mobile/ios
//
//  凭据存储。凭据只进 Keychain（不落 UserDefaults、不进日志）。
//
//  每次 SecItem 调用都检查状态码：模拟器上未签名 / 缺 keychain-access-groups 时
//  SecItemAdd 返回 -34018（errSecMissingEntitlement）。过去这里把返回值直接丢掉，
//  于是「写失败」被吞成「静默成功」，表现为登录态存不住、界面也不动。
//  现在一律显式失败并带上原因。
//

import Foundation
import Security

/// Keychain 访问失败。
enum KeychainError: LocalizedError {
    /// 系统返回了非预期状态码。
    case status(OSStatus, operation: String)
    /// 凭据无法序列化。
    case encoding

    var errorDescription: String? {
        switch self {
        case let .status(code, operation):
            let text = SecCopyErrorMessageString(code, nil) as String? ?? "OSStatus \(code)"
            return "Keychain \(operation) 失败：\(text)（\(code)）"
        case .encoding:
            return "凭据无法序列化。"
        }
    }
}

enum KeychainStore {
    private static let service = "ai.ivec.desktop"

    /// 保存字符串凭据。
    static func set(_ value: String, for account: String) throws {
        let query: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
            kSecAttrAccount as String: account
        ]
        let deleted = SecItemDelete(query as CFDictionary)
        guard deleted == errSecSuccess || deleted == errSecItemNotFound else {
            NSLog("[aic] keychain delete(%@) failed: %d", account, deleted)
            throw KeychainError.status(deleted, operation: "delete")
        }

        var attributes = query
        attributes[kSecValueData as String] = Data(value.utf8)
        attributes[kSecAttrAccessible as String] = kSecAttrAccessibleAfterFirstUnlock
        let added = SecItemAdd(attributes as CFDictionary, nil)
        guard added == errSecSuccess else {
            NSLog("[aic] keychain add(%@) failed: %d", account, added)
            throw KeychainError.status(added, operation: "add")
        }
    }

    /// 读取字符串凭据；没有这条记录返回 nil，其它错误抛出。
    static func get(_ account: String) throws -> String? {
        let query: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
            kSecAttrAccount as String: account,
            kSecReturnData as String: true,
            kSecMatchLimit as String: kSecMatchLimitOne
        ]
        var item: CFTypeRef?
        let status = SecItemCopyMatching(query as CFDictionary, &item)
        if status == errSecItemNotFound { return nil }
        guard status == errSecSuccess else {
            NSLog("[aic] keychain read(%@) failed: %d", account, status)
            throw KeychainError.status(status, operation: "read")
        }
        guard let data = item as? Data else { return nil }
        return String(data: data, encoding: .utf8)
    }

    /// 删除凭据。
    static func remove(_ account: String) throws {
        let query: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
            kSecAttrAccount as String: account
        ]
        let status = SecItemDelete(query as CFDictionary)
        guard status == errSecSuccess || status == errSecItemNotFound else {
            NSLog("[aic] keychain remove(%@) failed: %d", account, status)
            throw KeychainError.status(status, operation: "remove")
        }
    }

    enum Account {
        /// 网页登录后的 auth 令牌。
        static let authToken = "auth.token"
        /// 会话 cookie 串（冷启动恢复登录态用）。
        static let sessionCookies = "auth.cookies"
    }
}
