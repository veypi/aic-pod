//
//  AuthModels.swift
//  aic-pod · mobile/ios
//
//  动态 Web 会话凭据模型与 JWT / Set-Cookie 解析。
//  令牌只从 HttpOnly Cookie 取，响应 body 只提供用户信息。
//

import Foundation

/// 登录用户（vbase 的 user 段）。
struct AuthUser: Codable, Equatable {
    var id: String
    var username: String?
    var nickname: String?

    var displayName: String { nickname ?? username ?? id }

    init(id: String, username: String? = nil, nickname: String? = nil) {
        self.id = id
        self.username = username
        self.nickname = nickname
    }
}

/// 会话凭据：access / refresh 两个动态 token（都来自 HttpOnly Cookie）。
struct AuthCredentials: Codable, Equatable {
    var accessToken: String
    var refreshToken: String
    var accessCookieName: String
    var refreshCookieName: String
    var user: AuthUser
    /// access token 的过期时刻。
    var expiresAt: Date
}

/// 对外公开的登录态：不含任何 token。
struct AuthState: Codable, Equatable {
    var signedIn: Bool
    var user: AuthUser?
    var expiresAt: Date

    static let signedOut = AuthState(signedIn: false, user: nil, expiresAt: .distantPast)

    /// 桥协议里的形状：{signedIn, user, expiresAt}。
    var dictionary: [String: Any] {
        var out: [String: Any] = [
            "signedIn": signedIn,
            "expiresAt": Int((expiresAt.timeIntervalSince1970 * 1000).rounded())
        ]
        if let user {
            var value: [String: Any] = ["id": user.id]
            if let username = user.username { value["username"] = username }
            if let nickname = user.nickname { value["nickname"] = nickname }
            out["user"] = value
        } else {
            out["user"] = NSNull()
        }
        return out
    }
}

/// JWT 解析：只读载荷，不验签（服务端才是验签方）。
enum JWT {
    /// 解出载荷；格式不对返回 nil。
    static func payload(_ token: String) -> [String: Any]? {
        let parts = token.split(separator: ".", omittingEmptySubsequences: false)
        guard parts.count == 3 else { return nil }
        var segment = String(parts[1])
            .replacingOccurrences(of: "-", with: "+")
            .replacingOccurrences(of: "_", with: "/")
        let remainder = segment.count % 4
        if remainder > 0 { segment += String(repeating: "=", count: 4 - remainder) }
        guard let data = Data(base64Encoded: segment),
              let object = try? JSONSerialization.jsonObject(with: data) as? [String: Any] else {
            return nil
        }
        return object
    }

    /// 用户身份：uid 或 sub，必须是 32 位小写十六进制。
    static func uid(_ token: String) -> String? {
        guard let payload = payload(token) else { return nil }
        let raw = (payload["uid"] as? String) ?? (payload["sub"] as? String) ?? ""
        guard raw.count == 32 else { return nil }
        let hex = raw.allSatisfy { $0.isHexDigit && !$0.isUppercase }
        return hex ? raw : nil
    }

    /// token 类型（access / refresh）。
    static func type(_ token: String) -> String? {
        payload(token)?["type"] as? String
    }

    /// access token 的过期时刻（exp 是秒级时间戳）。
    static func expiresAt(_ token: String, fallback: Date) -> Date {
        guard let payload = payload(token), let exp = payload["exp"] as? Double else { return fallback }
        return Date(timeIntervalSince1970: exp)
    }
}

/// 一条 Set-Cookie。
struct ParsedCookie {
    var name: String
    var value: String
    var path: String
    var domain: String
    var secure: Bool
    var httpOnly: Bool
    var expires: Date?

    /// 转成 WebView / URLSession 能用的 cookie。
    func httpCookie(host: String) -> HTTPCookie? {
        var properties: [HTTPCookiePropertyKey: Any] = [
            .name: name,
            .value: value,
            .domain: domain.isEmpty ? host : domain,
            .path: path.isEmpty ? "/" : path
        ]
        if secure { properties[.secure] = true }
        if let expires { properties[.expires] = expires }
        return HTTPCookie(properties: properties)
    }
}

/// Set-Cookie 头解析。
enum SetCookie {
    /// 把可能合并在一起的 Set-Cookie 头拆成多条（Expires 里的逗号不会被误切）。
    static func split(_ raw: String) -> [String] {
        let pattern = ",\\s*(?=[!#$%&'*+\\-.^_`|~0-9A-Za-z]+=)"
        guard let regex = try? NSRegularExpression(pattern: pattern) else {
            return raw.split(separator: ",").map { String($0).trimmingCharacters(in: .whitespaces) }
        }
        var result: [String] = []
        var cursor = raw.startIndex
        let range = NSRange(raw.startIndex..., in: raw)
        for match in regex.matches(in: raw, range: range) {
            guard let hit = Range(match.range, in: raw) else { continue }
            let piece = raw[cursor..<hit.lowerBound].trimmingCharacters(in: .whitespaces)
            if !piece.isEmpty { result.append(piece) }
            cursor = hit.upperBound
        }
        let tail = raw[cursor...].trimmingCharacters(in: .whitespaces)
        if !tail.isEmpty { result.append(tail) }
        return result
    }

    /// 解析一条 Set-Cookie。
    static func parse(_ line: String) -> ParsedCookie? {
        let parts = line.split(separator: ";", omittingEmptySubsequences: false)
            .map { $0.trimmingCharacters(in: .whitespaces) }
        guard let head = parts.first else { return nil }
        let pair = head.split(separator: "=", maxSplits: 1, omittingEmptySubsequences: false)
        guard pair.count == 2 else { return nil }
        var cookie = ParsedCookie(
            name: String(pair[0]),
            value: String(pair[1]),
            path: "/",
            domain: "",
            secure: false,
            httpOnly: false,
            expires: nil
        )
        for part in parts.dropFirst() {
            let kv = part.split(separator: "=", maxSplits: 1, omittingEmptySubsequences: false)
            let key = kv.first.map { String($0).lowercased() } ?? ""
            let value = kv.count > 1 ? String(kv[1]) : ""
            switch key {
            case "path": if value.hasPrefix("/") { cookie.path = value }
            case "domain": cookie.domain = value
            case "secure": cookie.secure = true
            case "httponly": cookie.httpOnly = true
            case "expires": cookie.expires = parseDate(value)
            default: break
            }
        }
        return cookie
    }

    /// 从 HTTP 响应里取出全部 Set-Cookie。
    static func cookies(from response: HTTPURLResponse) -> [ParsedCookie] {
        var raw: [String] = []
        for (key, value) in response.allHeaderFields {
            guard let name = key as? String, name.lowercased() == "set-cookie" else { continue }
            if let list = value as? [String] {
                raw.append(contentsOf: list)
            } else if let single = value as? String {
                raw.append(single)
            }
        }
        return raw
            .flatMap { split($0) }
            .compactMap { parse($0) }
    }

    private static func parseDate(_ value: String) -> Date? {
        let formatter = DateFormatter()
        formatter.locale = Locale(identifier: "en_US_POSIX")
        formatter.dateFormat = "EEE, dd MMM yyyy HH:mm:ss zzz"
        return formatter.date(from: value)
    }
}
