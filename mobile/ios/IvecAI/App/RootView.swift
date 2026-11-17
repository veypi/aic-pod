//
//  RootView.swift
//  aic-pod · mobile/ios
//
//  根视图：未登录显示原生登录/注册页，登录后进工作区（webview 全屏）。
//

import SwiftUI

struct RootView: View {
    @ObservedObject var config: ServerConfig
    @StateObject private var workspace: WorkspaceViewModel
    @State private var consented = false
    @State private var isSignedIn = false
    @State private var sessionCookies: [HTTPCookie] = []
    @State private var showSettings = false

    init(config: ServerConfig) {
        self.config = config
        _workspace = StateObject(wrappedValue: WorkspaceViewModel(config: config))
    }

    var body: some View {
        ZStack {
            if isSignedIn {
                WorkspaceView(model: workspace, config: config, cookies: sessionCookies)
                    .disabled(!consented)
            } else {
                LoginView(
                    config: config,
                    onLogin: { cookies in
                        sessionCookies = cookies
                        isSignedIn = true
                    },
                    onOpenSettings: { showSettings = true }
                )
            }

            if isSignedIn && !consented {
                ConsentView(config: config) {
                    consented = true
                }
                .transition(.opacity)
            }
        }
        .animation(.easeInOut(duration: 0.2), value: consented)
        .sheet(isPresented: $showSettings) {
            SettingsView(model: SettingsViewModel(config: config)) {
                workspace.reload()
            }
            .presentationDetents([.large])
        }
        .onAppear {
            consented = config.consent != nil
            // 冷启动：Keychain 里存着上次的会话 cookie，直接恢复登录态。
            if let saved = KeychainStore.get(KeychainStore.Account.sessionCookies) {
                let cookies = Self.restoreCookies(saved)
                if !cookies.isEmpty {
                    sessionCookies = cookies
                    isSignedIn = true
                }
            }
        }
    }

    /// 还原上次会话的 cookie（冷启动免登录）。
    static func restoreCookies(_ raw: String) -> [HTTPCookie] {
        raw.split(separator: "\n").compactMap { line in
            let parts = line.split(separator: "; ").map(String.init)
            guard let head = parts.first else { return nil }
            let pair = head.split(separator: "=", maxSplits: 1).map(String.init)
            guard pair.count == 2 else { return nil }
            var properties: [HTTPCookiePropertyKey: Any] = [
                .name: pair[0],
                .value: pair[1],
                .domain: configDomainFallback,
                .path: "/"
            ]
            for part in parts.dropFirst() {
                if part.hasPrefix("Domain=") { properties[.domain] = String(part.dropFirst(7)) }
                if part.hasPrefix("Path=") { properties[.path] = String(part.dropFirst(5)) }
            }
            return HTTPCookie(properties: properties)
        }
    }

    /// 还原 cookie 时缺少域名时的占位（会被上面 parts 里的 Domain 覆盖）。
    private static let configDomainFallback = "localhost"
}
