//
//  L10nStore.swift
//  aic-pod · mobile/ios
//
//  壳侧文案。唯一来源是 aic-pod/client-bridge/l10n.json：
//  Desktop 直接读 JSON，iOS 构建时把同一份 JSON 打进 App 包。
//  壳侧不再维护第二份文案表。
//

import Foundation

enum L10nStore {
    /// 语言 → 文案表。语言码与 l10n.json 的键一致（zh-CN / en-US）。
    static let table: [AppLanguage: [String: String]] = load()

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

    /// 读取 App 包里的 l10n.json。
    private static func load() -> [AppLanguage: [String: String]] {
        guard let url = Bundle.main.url(forResource: "l10n", withExtension: "json"),
              let data = try? Data(contentsOf: url),
              let root = try? JSONSerialization.jsonObject(with: data) as? [String: [String: String]] else {
            return [:]
        }
        var result: [AppLanguage: [String: String]] = [:]
        for (code, values) in root {
            guard let language = AppLanguage(rawValue: code) else { continue }
            result[language] = values
        }
        return result
    }
}
