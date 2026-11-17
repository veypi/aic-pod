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

    var body: some Scene {
        WindowGroup {
            RootView(config: config)
                .brandTheme(config.theme)
        }
    }
}
