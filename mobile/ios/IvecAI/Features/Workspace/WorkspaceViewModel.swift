//
//  WorkspaceViewModel.swift
//  aic-pod · mobile/ios
//
//  工作区状态：当前服务地址、页面加载状态、以及页面交给壳的凭据。
//

import Foundation
import SwiftUI

@MainActor
final class WorkspaceViewModel: ObservableObject {
    @Published private(set) var isLoading = true
    @Published private(set) var loadError: String?
    /// 壳 → 页面的待发消息（变化时触发一次下发）。
    @Published var pendingPost: String?
    /// 当前工作区路径（底栏切换时整页跳转到对应路由）。
    @Published var workspacePath: String = AppConfig.workspacePath

    let config: ServerConfig

    init(config: ServerConfig) {
        self.config = config
    }

    /// 工作区地址：服务域名 + 当前路径。
    var workspaceURL: URL {
        config.server.sitePath(workspacePath)
    }

    /// 切换底栏：跳到平台对应路由（/、/skills、/fs）。
    func navigate(to path: String) {
        guard path != workspacePath else { return }
        workspacePath = path
        loadError = nil
        isLoading = true
    }

    /// 处理页面消息：令牌存 Keychain，登出清凭据，JS 报错显示在错误面板。
    func handle(_ message: ShellInboundMessage) {
        switch message {
        case let .authToken(token):
            KeychainStore.set(token, for: KeychainStore.Account.authToken)
        case .signedOut:
            KeychainStore.remove(KeychainStore.Account.authToken)
        case let .pageError(text):
            loadError = text
            isLoading = false
        }
    }

    /// 账号已注销：清本机凭据并通知页面。
    func accountDeleted() {
        KeychainStore.remove(KeychainStore.Account.authToken)
        pendingPost = ShellOutboundMessage.accountDeleted.json
    }

    /// 服务地址变更后重新加载工作区。
    func reload() {
        loadError = nil
        isLoading = true
    }

    /// 页面加载完成：撤掉加载遮罩。
    func loadFinished() {
        loadError = nil
        isLoading = false
    }

    /// 页面加载失败：显示原因。
    func loadFailed(_ message: String) {
        loadError = message
        isLoading = false
    }
}
