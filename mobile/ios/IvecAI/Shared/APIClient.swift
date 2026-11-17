//
//  APIClient.swift
//  aic-pod · mobile/ios
//
//  壳侧直连 vbase 的公共接口：认证配置与验证码。
//  登录 / 刷新 / 登出 / 注销由原生认证管理器负责（它持有 token）。
//

import Foundation

enum APIError: LocalizedError {
    case server(status: Int, message: String)

    var errorDescription: String? {
        switch self {
        case let .server(status, message):
            return message.isEmpty ? "服务返回 \(status)" : message
        }
    }
}

struct APIClient {
    var server: URL
    var session: URLSession = .shared

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
