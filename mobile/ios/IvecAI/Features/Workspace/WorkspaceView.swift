//
//  WorkspaceView.swift
//  aic-pod · mobile/ios
//
//  工作区：平台会话页面由 WKWebView 承载，原生只保留一条轻量顶栏与底部工具条。
//

import SwiftUI

struct WorkspaceView: View {
    @ObservedObject var model: WorkspaceViewModel
    @ObservedObject var config: ServerConfig
    /// 登录会话 cookie（写入 webview）。
    var cookies: [HTTPCookie] = []
    @State private var showSettings = false

    var body: some View {
        content
            .background(Color.brandBackground)
            .sheet(isPresented: $showSettings) {
                SettingsView(model: SettingsViewModel(config: model.config)) {
                    model.reload()
                }
                .presentationDetents([.large])
            }
    }

    private var content: some View {
        ZStack {
            WebViewContainer(
                url: model.workspaceURL,
                onMessage: { message in
                    Task { @MainActor in model.handle(message) }
                },
                onLoad: {
                    Task { @MainActor in model.loadFinished() }
                },
                onFail: { message in
                    Task { @MainActor in model.loadFailed(message) }
                },
                postToPage: model.pendingPost,
                cookies: cookies
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
                        Text("重试")
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

/// 品牌标记：纯黑环 + A，浅底。矢量绘制，随系统深浅色反相由外层处理。
struct BrandMark: View {
    var size: CGFloat = 24

    var body: some View {
        ZStack {
            RoundedRectangle(cornerRadius: size * 0.22, style: .continuous)
                .fill(Brand.field)
            HStack(spacing: size * 0.04) {
                Circle()
                    .trim(from: 0.08, to: 0.86)
                    .stroke(Color.black, style: StrokeStyle(lineWidth: size * 0.09, lineCap: .round))
                    .frame(width: size * 0.46, height: size * 0.46)
                Text("A")
                    .font(.system(size: size * 0.34, weight: .semibold, design: .rounded))
                    .foregroundStyle(.black)
                    .offset(x: -size * 0.02)
            }
        }
        .frame(width: size, height: size)
    }
}
