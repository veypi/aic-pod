//
//  OAuthWebSheet.swift
//  aic-pod · mobile/ios
//
//  应用内三方授权：用 WKWebView 加载提供商授权页，回调落回平台域名后
//  把会话 cookie 交给原生认证管理器接管（与 Desktop 的授权窗同一思路）。
//

import SwiftUI
import WebKit

/// 一个待打开的授权地址（URL 本身不满足 Identifiable）。
struct OAuthTarget: Identifiable {
    let id = UUID()
    let url: URL
}

struct OAuthWebSheet: View {
    let target: OAuthTarget
    /// 平台域名：导航回到它就认为授权完成。
    let platformHost: String?
    let cancelTitle: String
    /// 授权完成：拿到会话 cookie。
    var onCookies: ([HTTPCookie]) -> Void

    @Environment(\.dismiss) private var dismiss

    var body: some View {
        NavigationStack {
            OAuthWebView(url: target.url, platformHost: platformHost) { cookies in
                onCookies(cookies)
                dismiss()
            }
            .ignoresSafeArea(edges: .bottom)
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button(cancelTitle) { dismiss() }
                }
            }
        }
    }
}

private struct OAuthWebView: UIViewRepresentable {
    let url: URL
    let platformHost: String?
    let onCookies: ([HTTPCookie]) -> Void

    func makeCoordinator() -> Coordinator {
        Coordinator(platformHost: platformHost, onCookies: onCookies)
    }

    func makeUIView(context: Context) -> WKWebView {
        let configuration = WKWebViewConfiguration()
        // 与工作区同一个 cookie 容器：授权拿到的会话后续直接可用。
        configuration.websiteDataStore = .default()
        let webView = WKWebView(frame: .zero, configuration: configuration)
        webView.navigationDelegate = context.coordinator
        webView.load(URLRequest(url: url))
        return webView
    }

    func updateUIView(_ webView: WKWebView, context: Context) {}

    final class Coordinator: NSObject, WKNavigationDelegate {
        let platformHost: String?
        let onCookies: ([HTTPCookie]) -> Void
        private var finished = false

        init(platformHost: String?, onCookies: @escaping ([HTTPCookie]) -> Void) {
            self.platformHost = platformHost
            self.onCookies = onCookies
        }

        func webView(_ webView: WKWebView, didFinish navigation: WKNavigation!) {
            guard !finished,
                  let host = webView.url?.host,
                  let platformHost,
                  host == platformHost else { return }
            webView.configuration.websiteDataStore.httpCookieStore.getAllCookies { [weak self] cookies in
                guard let self, !self.finished else { return }
                // 回调页本身也会落在平台域名上，但那时页面还在调 /api/auth/callback；
                // 没有 access cookie 就保持窗口，等页跳转后下一次 didFinish 再读。
                let hasSession = cookies.contains { $0.name.lowercased().hasSuffix("access") }
                guard hasSession else { return }
                self.finished = true
                DispatchQueue.main.async { self.onCookies(cookies) }
            }
        }
    }
}
