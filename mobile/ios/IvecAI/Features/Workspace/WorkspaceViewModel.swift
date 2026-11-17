//
//  WorkspaceViewModel.swift
//  aic-pod · mobile/ios
//
//  工作区状态：当前服务地址、页面加载状态、宿主桥。
//  登录态在原生认证管理器里，网页不再把令牌交给壳。
//

import Foundation
import SwiftUI

@MainActor
final class WorkspaceViewModel: ObservableObject {
    @Published private(set) var isLoading = true
    @Published private(set) var loadError: String?
    /// 当前工作区路径（底栏切换时整页跳转到对应路由）。
    @Published var workspacePath: String = AppConfig.workspacePath

    let config: ServerConfig
    let auth: AuthManager
    let bridge: AicHostBridge

    init(config: ServerConfig, coordinator: AppCoordinator) {
        self.config = config
        self.auth = coordinator.auth
        self.bridge = coordinator.bridge
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

    /// 页面里的 JS 报错：显示在错误面板。
    func pageError(_ message: String) {
        loadError = message
        isLoading = false
    }
}
