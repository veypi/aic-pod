//
//  WorkspaceView.swift
//  aic-pod · mobile/ios
//
//  工作区：平台会话页面由 WKWebView 承载，原生只保留一条轻量底栏。
//

import SwiftUI

struct WorkspaceView: View {
    @ObservedObject var model: WorkspaceViewModel
    @ObservedObject var config: ServerConfig
    @ObservedObject var coordinator: AppCoordinator
    @State private var showSettings = false

    var body: some View {
        VStack(spacing: 0) {
            content
            bottomBar
        }
        .background(Color.brandBackground)
        .sheet(isPresented: $showSettings) {
            SettingsView(model: SettingsViewModel(config: model.config, auth: model.auth)) {
                model.reload()
            }
            .presentationDetents([.large])
        }
    }

    private var content: some View {
        ZStack {
            WebViewContainer(
                url: model.workspaceURL,
                auth: model.auth,
                bridge: model.bridge,
                onLoad: {
                    Task { @MainActor in model.loadFinished() }
                },
                onFail: { message in
                    Task { @MainActor in model.loadFailed(message) }
                },
                onPageError: { message in
                    Task { @MainActor in model.pageError(message) }
                }
            )
            if model.isLoading {
                ProgressView()
                    .controlSize(.large)
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
                    .background(Color.brandBackground.opacity(0.85))
            }
            if let error = model.loadError {
                VStack(spacing: 8) {
                    Image(systemName: "exclamationmark.triangle")
                        .font(.system(size: 26))
                        .foregroundStyle(.secondary)
                    Text(error)
                        .font(.system(size: 13))
                        .foregroundStyle(.secondary)
                        .multilineTextAlignment(.center)
                    Button {
                        model.reload()
                    } label: {
                        Text(config.t("common.retry"))
                            .font(.system(size: 14, weight: .medium))
                    }
                }
                .padding(20)
                .frame(maxWidth: .infinity, maxHeight: .infinity)
                .background(Color.brandBackground)
            }
        }
    }

    private var bottomBar: some View {
        HStack {
            tabButton(config.t("workspace.session"), "circle.dashed.inset.filled", path: "/")
            tabButton(config.t("workspace.skills"), "square.grid.2x2", path: "/skills")
            tabButton(config.t("workspace.files"), "doc.text", path: "/fs")
            Button {
                showSettings = true
            } label: {
                barItem(config.t("workspace.settings"), "gearshape")
            }
            .buttonStyle(.plain)
        }
        .padding(.vertical, 6)
        .background(Color.brandSurface)
    }

    private func tabButton(_ title: String, _ symbol: String, path: String) -> some View {
        Button {
            model.navigate(to: path)
        } label: {
            barItem(title, symbol, active: model.workspacePath == path)
        }
        .buttonStyle(.plain)
    }

    private func barItem(_ title: String, _ symbol: String, active: Bool = false) -> some View {
        VStack(spacing: 2) {
            Image(systemName: symbol).font(.system(size: 17))
            Text(title).font(.system(size: 10))
        }
        .foregroundStyle(active ? Color.brandPrimary : Color.brandSecondary)
        .frame(maxWidth: .infinity)
    }
}
