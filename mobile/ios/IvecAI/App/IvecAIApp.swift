//
//  IvecAIApp.swift
//  aic-pod · mobile/ios
//
//  应用入口。
//

import SwiftUI

@main
struct IvecAIApp: App {
    @StateObject private var config = ServerConfig.shared
    @StateObject private var coordinator: AppCoordinator

    init() {
        let config = ServerConfig.shared
        _config = StateObject(wrappedValue: config)
        _coordinator = StateObject(wrappedValue: AppCoordinator(server: config.server))
    }

    var body: some Scene {
        WindowGroup {
            RootView(config: config, coordinator: coordinator)
                .brandTheme(config.theme)
                .onChange(of: config.server) { _, server in
                    coordinator.switchServer(server)
                }
        }
    }
}
