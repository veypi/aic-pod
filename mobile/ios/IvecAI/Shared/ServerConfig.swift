//
//  ServerConfig.swift
//  aic-pod · mobile/ios
//
//  服务域名、主题、语言等壳侧本地配置。放在 UserDefaults：
//  这些是「这台设备的偏好」，不是凭据；凭据走 Keychain。
//

import Foundation
import SwiftUI

extension URL {
    /// 拼接站点内的绝对路径（保留斜杠）。
    ///
    /// 不能用 URL.appending(path:)：它把整个字符串当单个路径组件，
    /// 斜杠会被编码成 %2F（/vb/api/config → /%2Fvb%2Fapi%2Fconfig，请求必然 404）。
    func sitePath(_ path: String) -> URL {
        URL(string: path, relativeTo: self)?.absoluteURL ?? self
    }
}

@MainActor
final class ServerConfig: ObservableObject {
    static let shared = ServerConfig()

    private enum Key {
        static let server = "ivec.server"
        static let theme = "ivec.theme"
        static let language = "ivec.language"
        static let consent = "ivec.consent"
    }

    /// 服务域名（协议、登录、工作区都在这里）。
    @Published var server: URL {
        didSet { defaults.set(server.absoluteString, forKey: Key.server) }
    }

    /// 主题：跟随系统 / 浅色 / 深色。
    @Published var theme: AppTheme {
        didSet { defaults.set(theme.rawValue, forKey: Key.theme) }
    }

    /// 界面语言：跟随系统 / 中文 / English。
    @Published var language: AppLanguage {
        didSet { defaults.set(language.rawValue, forKey: Key.language) }
    }

    /// 首启协议同意状态：记录同意时间与当时的协议清单。
    @Published var consent: ConsentRecord? {
        didSet {
            if let consent {
                defaults.set(try? JSONEncoder().encode(consent), forKey: Key.consent)
            } else {
                defaults.removeObject(forKey: Key.consent)
            }
        }
    }

    /// 实际生效的语言：跟随系统时按当前区域判定。
    var resolvedLanguage: AppLanguage {
        switch language {
        case .system:
            let code = Locale.current.language.languageCode?.identifier ?? "zh"
            return code == "en" ? .en : .zh
        case .zh: return .zh
        case .en: return .en
        }
    }

    /// 取本地化文案（视图里读它，语言变化会触发重绘）。
    func t(_ key: String) -> String {
        L10n.text(key, language: resolvedLanguage)
    }

    /// 取带一个占位符的本地化文案。
    func t(_ key: String, _ argument: String) -> String {
        L10n.text(key, language: resolvedLanguage, argument)
    }

    private let defaults: UserDefaults

    init(defaults: UserDefaults = .standard) {
        self.defaults = defaults

        let raw = defaults.string(forKey: Key.server) ?? ""
        server = URL(string: raw) ?? AppConfig.defaultServer
        theme = AppTheme(rawValue: defaults.string(forKey: Key.theme) ?? "") ?? .system
        language = AppLanguage(rawValue: defaults.string(forKey: Key.language) ?? "") ?? .system

        if let data = defaults.data(forKey: Key.consent),
           let record = try? JSONDecoder().decode(ConsentRecord.self, from: data) {
            consent = record
        } else {
            consent = nil
        }
    }

    /// 同意记录：勾选时间 + 当时展示的协议清单（合规留痕的本地一半）。
    struct ConsentRecord: Codable, Equatable {
        var agreedAt: Date
        var documents: [LegalDocument]
    }
}

enum AppTheme: String, CaseIterable, Identifiable {
    case system
    case light
    case dark

    var id: String { rawValue }

    /// 展示名（本地化 key）。
    var titleKey: String {
        switch self {
        case .system: return "settings.follow_system"
        case .light: return "settings.light"
        case .dark: return "settings.dark"
        }
    }
}

enum AppLanguage: String, CaseIterable, Identifiable {
    case system
    case zh
    case en

    var id: String { rawValue }

    /// 展示名（本地化 key）。
    var titleKey: String {
        switch self {
        case .system: return "settings.follow_system"
        case .zh: return "settings.chinese"
        case .en: return "settings.english"
        }
    }
}
