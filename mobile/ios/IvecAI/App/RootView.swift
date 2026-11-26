//
//  RootView.swift
//  aic-pod · mobile/ios
//
//  根视图：未登录显示原生登录页，登录后进工作区（WKWebView 全屏）。
//  登录态由原生认证管理器持有，网页只是它的镜像。
//

import SwiftUI

struct RootView: View {
    @ObservedObject var config: ServerConfig
    @ObservedObject var coordinator: AppCoordinator
    @ObservedObject private var auth: AuthManager
    @StateObject private var workspace: WorkspaceViewModel
    @State private var consented = false
    @State private var showSettings = false

    init(config: ServerConfig, coordinator: AppCoordinator) {
        self.config = config
        self.coordinator = coordinator
        _auth = ObservedObject(wrappedValue: coordinator.auth)
        _workspace = StateObject(wrappedValue: WorkspaceViewModel(config: config, coordinator: coordinator))
    }

    /// 是否停在原生登录页：没有登录态，或网页要求回到原生登录。
    private var showsLogin: Bool {
        !auth.isSignedIn || coordinator.requiresNativeLogin
    }

    var body: some View {
        ZStack {
            if showsLogin {
                LoginView(
                    config: config,
                    auth: auth,
                    onOpenSettings: { showSettings = true }
                )
            } else {
                WorkspaceView(model: workspace, config: config, coordinator: coordinator)
                    .disabled(!consented)
            }

            if !showsLogin && !consented {
                ConsentView(config: config) {
                    consented = true
                }
                .transition(.opacity)
            }
        }
        .animation(.easeInOut(duration: 0.2), value: consented)
        .sheet(isPresented: $showSettings) {
            SettingsView(model: SettingsViewModel(config: config, auth: auth)) {
                workspace.reload()
            }
            .presentationDetents([.large])
        }
        .onAppear {
            consented = config.consent != nil
        }
        .onChange(of: auth.isSignedIn) { _, signedIn in
            NSLog("[aic] rootview isSignedIn -> %@ (requiresNativeLogin=%@)",
                  signedIn ? "true" : "false",
                  coordinator.requiresNativeLogin ? "true" : "false")
            if signedIn {
                coordinator.didSignIn()
            }
        }
        .onChange(of: coordinator.requiresNativeLogin) { _, required in
            NSLog("[aic] rootview requiresNativeLogin -> %@", required ? "true" : "false")
        }
    }
}
