//
//  APIClient.swift
//  aic-pod · mobile/ios
//
//  壳侧直连 vbase 的少量接口：登出与自助注销账号。
//  令牌来自 Keychain（网页登录后由页面交给壳）。
//

import Foundation

enum APIError: LocalizedError {
    case noToken
    case server(status: Int, message: String)

    var errorDescription: String? {
        switch self {
        case .noToken:
            return "未登录，无法执行该操作。"
        case let .server(status, message):
            return message.isEmpty ? "服务返回 \(status)" : message
        }
    }
}

struct APIClient {
    var server: URL
    var session: URLSession = .shared

    /// 自助注销账号：DELETE /vb/api/auth/account
    /// 成功后会话、凭据与账号数据按隐私政策处理。
    func deleteAccount() async throws {
        try await authorizedRequest(path: AppConfig.accountDeletePath, method: "DELETE")
    }

    /// 登出：POST /vb/api/auth/logout（保留账号，仅吊销本次会话）。
    func logout() async throws {
        try await authorizedRequest(path: "/vb/api/auth/logout", method: "POST")
    }

    private func authorizedRequest(path: String, method: String) async throws {
        guard let token = KeychainStore.get(KeychainStore.Account.authToken), !token.isEmpty else {
            throw APIError.noToken
        }
        var request = URLRequest(url: server.sitePath(path))
        request.httpMethod = method
        request.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        request.setValue("application/json", forHTTPHeaderField: "Accept")

        let (data, response) = try await session.data(for: request)
        guard let http = response as? HTTPURLResponse else {
            throw URLError(.badServerResponse)
        }
        guard (200..<300).contains(http.statusCode) else {
            let message = (try? JSONDecoder().decode(ErrorPayload.self, from: data))?.message ?? ""
            throw APIError.server(status: http.statusCode, message: message)
        }
    }

    /// 登录：成功后会话 cookie 落在 URLSession 的共享存储里。
    /// 平台用 cookie 承载令牌（浏览器只有一个 cookie 位），因此把 cookie 写进
    /// WKWebView 的 cookie 存储，网页即认这个登录态。
    @discardableResult
    func login(username: String, password: String) async throws -> [HTTPCookie] {
        try await post(path: "/vb/api/auth/login", body: ["username": username, "password": password])
        return sessionCookies()
    }

    /// 注册：返回会话 cookie（未注册成功时抛错）。
    @discardableResult
    func register(username: String, password: String, email: String? = nil, phone: String? = nil,
                  emailCode: String? = nil, phoneCode: String? = nil) async throws -> [HTTPCookie] {
        var body: [String: String] = ["username": username, "password": password]
        if let email, !email.isEmpty { body["email"] = email }
        if let phone, !phone.isEmpty { body["phone"] = phone }
        if let emailCode, !emailCode.isEmpty { body["email_code"] = emailCode }
        if let phoneCode, !phoneCode.isEmpty { body["phone_code"] = phoneCode }
        try await post(path: "/vb/api/auth/register", body: body)
        return sessionCookies()
    }

    /// 验证码登录：type = email | sms。
    @discardableResult
    func loginWithCode(type: String, target: String, code: String) async throws -> [HTTPCookie] {
        try await post(path: "/vb/api/auth/login/code", body: ["type": type, "target": target, "code": code])
        return sessionCookies()
    }

    /// 发送验证码：purpose = login | register。
    func sendVerificationCode(type: String, target: String, purpose: String) async throws {
        try await post(path: "/vb/api/verification/send", body: ["type": type, "target": target, "purpose": purpose])
    }

    /// 认证相关配置（登录方式、注册要求、邮件/短信通道是否可用）。
    func fetchAuthConfig() async throws -> AuthConfig {
        let url = server.sitePath(AppConfig.configPath)
        let (data, response) = try await session.data(from: url)
        guard let http = response as? HTTPURLResponse, (200..<300).contains(http.statusCode) else {
            throw URLError(.badServerResponse)
        }
        guard let root = try JSONSerialization.jsonObject(with: data) as? [String: Any],
              let values = root["values"] as? [String: Any],
              let vb = values["vb"] as? [String: Any] else {
            return AuthConfig()
        }
        var config = AuthConfig()
        let methods = vb["auth.login.methods"] as? [String] ?? ["password"]
        config.passwordLoginEnabled = methods.contains("password")
        config.emailCodeEnabled = methods.contains("email_code") && (vb["email.enabled"] as? Bool ?? false)
        config.smsCodeEnabled = methods.contains("phone_code") && (vb["sms.enabled"] as? Bool ?? false)
        config.requireEmail = vb["auth.registration.require_email"] as? Bool ?? false
        config.requirePhone = vb["auth.registration.require_phone"] as? Bool ?? false
        config.emailEnabled = vb["email.enabled"] as? Bool ?? false
        config.smsEnabled = vb["sms.enabled"] as? Bool ?? false
        return config
    }

    /// 当前服务域名下的会话 cookie。
    func sessionCookies() -> [HTTPCookie] {
        guard let host = server.host else { return [] }
        return HTTPCookieStorage.shared.cookies?.filter { $0.domain.contains(host) } ?? []
    }

    /// 从 cookie 里取出令牌值（cookie 名里带 token 的那个）。
    static func token(in cookies: [HTTPCookie]) -> String? {
        cookies.first(where: { $0.name.lowercased().contains("token") })?.value
    }

    private func post(path: String, body: [String: String]) async throws {
        let url = server.sitePath(path)
        var request = URLRequest(url: url)
        request.httpMethod = "POST"
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.setValue("application/json", forHTTPHeaderField: "Accept")
        request.httpBody = try JSONSerialization.data(withJSONObject: body)

        let (data, response) = try await session.data(for: request)
        guard let http = response as? HTTPURLResponse else {
            throw URLError(.badServerResponse)
        }
        guard (200..<300).contains(http.statusCode) else {
            let message = (try? JSONDecoder().decode(ErrorPayload.self, from: data))?.message ?? ""
            throw APIError.server(status: http.statusCode, message: message)
        }
    }

    private struct ErrorPayload: Decodable {
        let message: String?
    }
}

/// 认证配置快照（来自 /vb/api/config 的 vb 段）。
struct AuthConfig {
    var passwordLoginEnabled = true
    var emailCodeEnabled = false
    var smsCodeEnabled = false
    var requireEmail = false
    var requirePhone = false
    var emailEnabled = false
    var smsEnabled = false

    /// 是否提供验证码登录。
    var codeLoginEnabled: Bool { emailCodeEnabled || smsCodeEnabled }

    /// 注册时邮箱/手机是否必填（对应字段才显示）。
    var registrationNeedsContact: Bool { requireEmail || requirePhone }
}
