//
//  AppCoordinator.swift
//  aic-pod · mobile/ios
//
//  壳内的总接线：认证管理器、宿主桥与根视图状态都挂在这里。
//  网页只通过 window.aicHost 说话，桥的方法实现集中在本文件。
//

import Foundation

/// 桥方法不支持时的错误（明确失败，不静默回退 Web）。
enum BridgeError: LocalizedError {
    case unsupported(String)

    var errorDescription: String? {
        switch self {
        case let .unsupported(method):
            return "unsupported aicHost method: \(method)"
        }
    }
}

@MainActor
final class AppCoordinator: ObservableObject {
    /// 网页要求回到原生登录页（登出后、或网页发现未登录）。
    @Published var requiresNativeLogin = false
    /// 网页上报的错误（工作区展示）。
    @Published var pageError: String?

    let auth: AuthManager
    let bridge = AicHostBridge()

    init(server: URL) {
        let auth = AuthManager(server: server)
        self.auth = auth
        bridge.handler = { [weak self] method, params in
            guard let self else { return [:] }
            AicLog.debug("bridge call \(method)")
            return try await self.handle(method, params)
        }
        auth.onStateChange = { [weak self] state in
            self?.bridge.emit("auth.changed", state.dictionary)
        }
        auth.restore()
    }

    /// 平台地址变化：换服务器即换登录档案。
    func switchServer(_ url: URL) {
        auth.switchServer(url)
        requiresNativeLogin = false
    }

    /// 登录成功后回到工作区。
    func didSignIn() {
        requiresNativeLogin = false
        pageError = nil
    }

    /// 宿主要求网页跳到某个路径（深链 / 通知）。
    func navigate(_ path: String) {
        bridge.emit("navigate", ["path": path])
    }

    // MARK: - 桥方法

    private func handle(_ method: String, _ params: [String: Any]) async throws -> [String: Any] {
        switch method {
        case "app_info":
            return [
                "version": Bundle.main.shortVersion,
                "platform": "ios",
                "build": Bundle.main.buildNumber
            ]
        case "auth_state":
            return auth.state.dictionary
        case "auth_refresh":
            return try await auth.refresh().dictionary
        case "auth_sign_out":
            await auth.signOut()
            requiresNativeLogin = true
            return ["ok": true]
        case "auth_open_login":
            AicLog.debug("bridge auth_open_login -> requiresNativeLogin = true")
            requiresNativeLogin = true
            return ["ok": true]
        case "account_delete":
            try await auth.deleteAccount()
            requiresNativeLogin = true
            return ["ok": true]
        case "page_error":
            pageError = params["message"] as? String
            return ["ok": true]
        default:
            AicLog.debug("bridge unsupported method \(method)")
            throw BridgeError.unsupported(method)
        }
    }
}
