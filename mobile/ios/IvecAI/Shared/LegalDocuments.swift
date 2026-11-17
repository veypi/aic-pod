//
//  LegalDocuments.swift
//  aic-pod · mobile/ios
//
//  协议地址模型与读取。数据源是后台的两个独立设置：
//  auth.legal.terms_url（用户协议）与 auth.legal.privacy_url（隐私政策）。
//  哪个配了就显示哪个；两份都没配则不要求同意。
//

import Foundation

/// 可用于 .sheet(item:) 的 URL 包装（URL 本身不满足 Identifiable）。
struct LegalLink: Identifiable {
    let id = UUID()
    let url: URL
}

/// 协议种类：决定展示名与设置项的对应关系。
enum LegalKind: String, Codable, Hashable {
    case terms
    case privacy

    /// 展示名（本地化 key；文案唯一来源是 client-bridge/l10n.json）。
    var titleKey: String {
        switch self {
        case .terms: return "login.terms"
        case .privacy: return "login.privacy"
        }
    }
}

/// 一份需要用户同意的协议。
struct LegalDocument: Identifiable, Hashable, Codable {
    var kind: LegalKind
    var url: String

    var id: String { kind.rawValue }

    /// 展示名（本地化 key；文案唯一来源是 client-bridge/l10n.json）。
    var titleKey: String { kind.titleKey }

    /// 站内相对路径补成绝对地址，便于在原生层打开。
    func absoluteURL(server: URL) -> URL? {
        if let direct = URL(string: url), direct.scheme != nil {
            return direct
        }
        return URL(string: url, relativeTo: server)?.absoluteURL
    }
}

enum LegalDocuments {
    /// 从公共配置接口读取两份协议地址；未配置的跳过。
    static func fetch(server: URL, session: URLSession = .shared) async throws -> [LegalDocument] {
        let url = server.sitePath(AppConfig.configPath)
        let (data, response) = try await session.data(from: url)
        guard let http = response as? HTTPURLResponse, (200..<300).contains(http.statusCode) else {
            throw URLError(.badServerResponse)
        }
        // 配置里既有字符串也有数组，直接用 JSONSerialization 取需要的两个字符串。
        guard let root = try JSONSerialization.jsonObject(with: data) as? [String: Any],
              let values = root["values"] as? [String: Any],
              let vb = values["vb"] as? [String: Any] else {
            return []
        }

        var docs: [LegalDocument] = []
        if let terms = normalized(vb[AppConfig.legalTermsKey] as? String) {
            docs.append(LegalDocument(kind: .terms, url: terms))
        }
        if let privacy = normalized(vb[AppConfig.legalPrivacyKey] as? String) {
            docs.append(LegalDocument(kind: .privacy, url: privacy))
        }
        return docs
    }

    /// 只接受站内相对路径与 http(s) 地址，其余按未配置处理。
    static func normalized(_ raw: String?) -> String? {
        guard let value = raw?.trimmingCharacters(in: .whitespacesAndNewlines), !value.isEmpty else {
            return nil
        }
        if value.hasPrefix("/") { return value }
        if value.hasPrefix("http://") || value.hasPrefix("https://") { return value }
        return nil
    }
}
