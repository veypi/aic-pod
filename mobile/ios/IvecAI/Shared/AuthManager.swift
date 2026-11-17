//
//  AuthManager.swift
//  aic-pod · mobile/ios
//
//  原生认证管理器：壳是登录态的唯一所有者。
//  它持有 access / refresh token（Keychain），统一执行登录、刷新、登出与注销；
//  WebView 里的 cookie 只是网页请求需要的镜像，刷新后由这里重新下发。
//  刷新只有一个执行者（singleflight），网页与 Pod 不会并发刷新。
//

import Foundation

/// 壳侧认证错误。
enum AuthError: LocalizedError {
    case notSignedIn
    case missingCredentials
    case invalidToken
    case accountMismatch
    case badResponse
    case server(status: Int, message: String)

    /// 需要重新登录（刷新时收到 401 或身份漂移）。
    var requiresRelogin: Bool {
        switch self {
        case .notSignedIn, .accountMismatch: return true
        case let .server(status, _): return status == 401
        default: return false
        }
    }

    var errorDescription: String? {
        switch self {
        case .notSignedIn: return "未登录，无法执行该操作。"
        case .missingCredentials: return "登录响应缺少 HttpOnly 会话凭证。"
        case .invalidToken: return "登录响应不是动态 Web access token。"
        case .accountMismatch: return "登录用户与访问凭证不一致。"
        case .badResponse: return "服务返回了无法识别的响应。"
        case let .server(_, message):
            return message.isEmpty ? "服务返回错误。" : message
        }
    }
}

/// 壳侧 HTTP 传输层：不使用系统 cookie 存储，Set-Cookie 由调用方自己处理。
struct AuthTransport {
    var session: URLSession

    static func live() -> AuthTransport {
        let configuration = URLSessionConfiguration.ephemeral
        configuration.httpCookieStorage = nil
        configuration.httpShouldSetCookies = false
        configuration.requestCachePolicy = .reloadIgnoringLocalCacheData
        return AuthTransport(session: URLSession(configuration: configuration))
    }

    struct Response {
        var data: Data
        var response: HTTPURLResponse
    }

    func request(server: URL, path: String, method: String,
                body: [String: String]?, headers: [String: String]) async throws -> Response {
        var request = URLRequest(url: server.sitePath(path))
        request.httpMethod = method
        request.setValue("application/json", forHTTPHeaderField: "Accept")
        if let body {
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
            request.httpBody = try JSONSerialization.data(withJSONObject: body)
        }
        for (key, value) in headers {
            request.setValue(value, forHTTPHeaderField: key)
        }
        let (data, response) = try await session.data(for: request)
        guard let http = response as? HTTPURLResponse else { throw AuthError.badResponse }
        guard (200..<300).contains(http.statusCode) else {
            let payload = try? JSONSerialization.jsonObject(with: data) as? [String: Any]
            let message = (payload?["message"] as? String)
                ?? ((payload?["error"] as? [String: Any])?["message"] as? String)
                ?? ""
            throw AuthError.server(status: http.statusCode, message: message)
        }
        return Response(data: data, response: http)
    }
}

@MainActor
final class AuthManager: ObservableObject {
    /// 当前登录态（不含 token）。
    @Published private(set) var state: AuthState = .signedOut
    /// 当前会话 cookie（壳 → WebView 的唯一来源）。
    @Published private(set) var sessionCookies: [HTTPCookie] = []
    /// 最近一次错误（视图展示用）。
    @Published var errorMessage: String?

    /// cookie 同步出口（工作区注入）。
    var cookieSink: (([HTTPCookie]) -> Void)?
    /// 状态变化回调（桥发 auth.changed）。
    var onStateChange: ((AuthState) -> Void)?

    private(set) var server: URL
    private var credentials: AuthCredentials?
    private var refreshTask: Task<AuthState, Error>?
    private let transport: AuthTransport

    init(server: URL, transport: AuthTransport = .live()) {
        self.server = server
        self.transport = transport
    }

    var isSignedIn: Bool { state.signedIn }

    /// 当前账户名（登录态里取）。
    var currentUser: AuthUser? { state.user }

    // MARK: - 服务器与恢复

    /// 切换平台地址：档案按服务器隔离，换地址即换档案。
    func switchServer(_ url: URL) {
        guard url != server else { return }
        server = url
        credentials = nil
        sessionCookies = []
        publish(state: .signedOut)
        restore()
    }

    /// 冷启动恢复：从 Keychain 读回凭据并同步 cookie。
    func restore() {
        guard let archive = AuthStore.load(server: server),
              let account = archive.accounts[archive.currentUID] else { return }
        credentials = account.credentials
        publish(state: AuthState(signedIn: true, user: account.credentials.user, expiresAt: account.credentials.expiresAt))
        publishCookies(cookies(for: account.credentials))
    }

    // MARK: - 登录

    func login(username: String, password: String) async throws {
        try await authenticate(path: "/vb/api/auth/login", body: ["username": username, "password": password])
    }

    func loginCode(type: String, target: String, code: String) async throws {
        try await authenticate(path: "/vb/api/auth/login/code", body: ["type": type, "target": target, "code": code])
    }

    func register(_ body: [String: String]) async throws {
        try await authenticate(path: "/vb/api/auth/register", body: body)
    }

    // MARK: - 刷新

    /// 统一刷新（singleflight）：并发调用共享同一次请求。
    @discardableResult
    func refresh() async throws -> AuthState {
        guard let current = credentials else { throw AuthError.notSignedIn }
        if let task = refreshTask { return try await task.value }

        let task = Task { @MainActor [weak self] () throws -> AuthState in
            guard let self else { throw AuthError.notSignedIn }
            defer { self.refreshTask = nil }
            do {
                let response = try await self.transport.request(
                    server: self.server,
                    path: "/vb/api/auth/refresh",
                    method: "POST",
                    body: [:],
                    headers: ["Cookie": "\(current.refreshCookieName)=\(current.refreshToken)"]
                )
                let next = try AuthManager.credentials(from: response, now: Date())
                guard next.user.id == current.user.id else { throw AuthError.accountMismatch }
                var archive = AuthStore.load(server: self.server) ?? AuthArchive()
                archive.accounts[next.user.id] = AuthArchive.Account(credentials: next)
                archive.currentUID = next.user.id
                AuthStore.save(archive, server: self.server)
                self.credentials = next
                self.publish(state: AuthState(signedIn: true, user: next.user, expiresAt: next.expiresAt))
                self.publishCookies(self.cookies(for: next))
                return self.state
            } catch {
                if (error as? AuthError)?.requiresRelogin == true {
                    self.clearLocal()
                }
                throw error
            }
        }
        refreshTask = task
        return try await task.value
    }

    /// 供后续 Host 接入使用的最新 access token。
    func accessToken() async throws -> String {
        guard let current = credentials else { throw AuthError.notSignedIn }
        if current.expiresAt <= Date().addingTimeInterval(30) {
            _ = try await refresh()
        }
        return credentials?.accessToken ?? current.accessToken
    }

    // MARK: - 登出与注销

    /// 登出：先吊销服务端会话，再清本机凭据与 cookie。
    func signOut() async {
        if let current = credentials {
            _ = try? await transport.request(
                server: server,
                path: "/vb/api/auth/logout",
                method: "POST",
                body: [:],
                headers: ["Authorization": "Bearer \(current.accessToken)"]
            )
        }
        clearLocal()
    }

    /// 注销账号：服务端删除账号，本机凭据一并清掉。
    func deleteAccount() async throws {
        guard let current = credentials else { throw AuthError.notSignedIn }
        let response = try await transport.request(
            server: server,
            path: AppConfig.accountDeletePath,
            method: "DELETE",
            body: nil,
            headers: ["Authorization": "Bearer \(current.accessToken)"]
        )
        _ = response
        clearLocal()
    }

    // MARK: - 内部

    private func authenticate(path: String, body: [String: String]) async throws {
        let response = try await transport.request(server: server, path: path, method: "POST", body: body, headers: [:])
        let next = try AuthManager.credentials(from: response, now: Date())
        var archive = AuthStore.load(server: server) ?? AuthArchive()
        let uid = next.user.id
        if !archive.currentUID.isEmpty, archive.currentUID != uid {
            archive.accounts.removeValue(forKey: archive.currentUID)
        }
        archive.accounts[uid] = AuthArchive.Account(credentials: next)
        archive.currentUID = uid
        AuthStore.save(archive, server: server)
        credentials = next
        publish(state: AuthState(signedIn: true, user: next.user, expiresAt: next.expiresAt))
        publishCookies(cookies(for: next))
    }

    /// 清本机登录态（凭据、cookie、状态）。
    private func clearLocal() {
        if var archive = AuthStore.load(server: server), !archive.currentUID.isEmpty {
            archive.accounts.removeValue(forKey: archive.currentUID)
            archive.currentUID = ""
            AuthStore.save(archive, server: server)
        }
        credentials = nil
        publish(state: .signedOut)
        publishCookies([])
    }

    private func publish(state: AuthState) {
        self.state = state
        onStateChange?(state)
    }

    private func publishCookies(_ cookies: [HTTPCookie]) {
        sessionCookies = cookies
        cookieSink?(cookies)
    }

    /// 凭据对应的 cookie（access + refresh）。
    private func cookies(for credentials: AuthCredentials) -> [HTTPCookie] {
        guard let host = server.host else { return [] }
        let secure = server.scheme == "https"
        let access = ParsedCookie(
            name: credentials.accessCookieName, value: credentials.accessToken,
            path: "/", domain: host, secure: secure, httpOnly: true, expires: credentials.expiresAt
        )
        let refresh = ParsedCookie(
            name: credentials.refreshCookieName, value: credentials.refreshToken,
            path: "/", domain: host, secure: secure, httpOnly: true, expires: nil
        )
        return [access, refresh].compactMap { $0.httpCookie(host: host) }
    }

    /// 从登录 / 刷新响应里取凭据：只认 HttpOnly 的 access + refresh cookie。
    static func credentials(from response: AuthTransport.Response, now: Date) throws -> AuthCredentials {
        let cookies = SetCookie.cookies(from: response.response)
        let access = cookies.first { $0.name.lowercased().hasSuffix("access") }
            ?? cookies.first { JWT.type($0.value) == "access" }
        let refresh = cookies.first { $0.name.lowercased().hasSuffix("refresh") }
            ?? cookies.first { JWT.type($0.value) == "refresh" }
        guard let access, let refresh,
              access.httpOnly, refresh.httpOnly,
              !access.value.isEmpty, !refresh.value.isEmpty else {
            throw AuthError.missingCredentials
        }
        let payload = JWT.payload(access.value)
        guard payload?["type"] as? String == "access", payload?["mod"] as? String == "inherit" else {
            throw AuthError.invalidToken
        }
        guard let uid = JWT.uid(access.value) else { throw AuthError.invalidToken }
        let refreshPayload = JWT.payload(refresh.value)
        guard refreshPayload?["type"] as? String == "refresh", JWT.uid(refresh.value) == uid else {
            throw AuthError.accountMismatch
        }
        let body = try? JSONSerialization.jsonObject(with: response.data) as? [String: Any]
        let info = body?["user"] as? [String: Any] ?? [:]
        if let bodyID = info["id"] as? String, bodyID != uid {
            throw AuthError.accountMismatch
        }
        let user = AuthUser(
            id: uid,
            username: info["username"] as? String,
            nickname: info["nickname"] as? String
        )
        let expiresIn = Double((body?["expires_in"] as? Int) ?? 900)
        let expiresAt = JWT.expiresAt(access.value, fallback: now.addingTimeInterval(expiresIn))
        return AuthCredentials(
            accessToken: access.value,
            refreshToken: refresh.value,
            accessCookieName: access.name,
            refreshCookieName: refresh.name,
            user: user,
            expiresAt: expiresAt
        )
    }
}
