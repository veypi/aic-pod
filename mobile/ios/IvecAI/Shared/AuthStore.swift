//
//  AuthStore.swift
//  aic-pod · mobile/ios
//
//  登录档案的持久化：按服务器隔离，按 uid 分账户。
//  凭据进 Keychain（不落 UserDefaults、不进日志）；登出只删当前账户的凭据，
//  其它账户的档案保留，便于切回时复用。
//

import Foundation

/// Keychain 里的登录档案。
struct AuthArchive: Codable {
    struct Account: Codable {
        var credentials: AuthCredentials
    }

    var version: Int = 1
    /// 当前登录账户；空表示本机没有登录态。
    var currentUID: String = ""
    var accounts: [String: Account] = [:]
}

enum AuthStore {
    /// 档案在 Keychain 里的 account 名（按服务器隔离）。
    private static func key(for server: URL) -> String {
        "auth.archive.\(server.host ?? server.absoluteString)"
    }

    /// 读档案；不存在、格式不对或 Keychain 不可用时返回 nil。
    static func load(server: URL) -> AuthArchive? {
        let raw: String?
        do {
            raw = try KeychainStore.get(key(for: server))
        } catch {
            AicLog.debug("auth archive load failed: \(String(describing: error))")
            return nil
        }
        guard let raw, let data = raw.data(using: .utf8),
              let archive = try? JSONDecoder().decode(AuthArchive.self, from: data) else {
            return nil
        }
        guard archive.version == 1, !archive.currentUID.isEmpty,
              archive.accounts[archive.currentUID] != nil else {
            return nil
        }
        return archive
    }

    /// 写档案（原子替换）；Keychain 写不进去时抛错，不静默吞。
    static func save(_ archive: AuthArchive, server: URL) throws {
        guard let data = try? JSONEncoder().encode(archive),
              let raw = String(data: data, encoding: .utf8) else {
            throw KeychainError.encoding
        }
        try KeychainStore.set(raw, for: key(for: server))
    }

    /// 清掉某台服务器的档案。
    static func clear(server: URL) {
        try? KeychainStore.remove(key(for: server))
    }
}
