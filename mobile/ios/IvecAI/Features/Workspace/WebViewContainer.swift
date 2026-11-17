//
//  WebViewContainer.swift
//  aic-pod · mobile/ios
//
//  WKWebView 封装：加载平台工作区，注入 window.aicHost 桥，
//  同步原生认证管理器下发的会话 cookie，并把站外链接交给系统。
//

import SwiftUI
import WebKit

struct WebViewContainer: UIViewRepresentable {
    let url: URL
    /// 原生认证管理器：会话 cookie 的唯一来源。
    let auth: AuthManager
    /// 宿主桥（window.aicHost）。
    let bridge: AicHostBridge
    /// 页面加载完成（遮罩应撤掉）。
    var onLoad: () -> Void = {}
    /// 页面加载失败。
    var onFail: (String) -> Void = { _ in }
    /// 页面里的 JS 报错。
    var onPageError: (String) -> Void = { _ in }

    func makeCoordinator() -> Coordinator {
        Coordinator(
            bridge: bridge,
            platformHost: auth.server.host,
            onLoad: onLoad,
            onFail: onFail
        )
    }

    func makeUIView(context: Context) -> WKWebView {
        let configuration = WKWebViewConfiguration()
        configuration.websiteDataStore = .default()
        configuration.defaultWebpagePreferences.allowsContentJavaScript = true

        let controller = WKUserContentController()
        controller.add(bridge, name: AicHostBridge.messageName)
        controller.addUserScript(WKUserScript(
            source: AicHostBridge.script,
            injectionTime: .atDocumentStart,
            forMainFrameOnly: true
        ))
        controller.addUserScript(WebViewContainer.errorProbeScript)
        configuration.userContentController = controller

        let webView = WKWebView(frame: .zero, configuration: configuration)
        webView.navigationDelegate = context.coordinator
        webView.allowsBackForwardNavigationGestures = true
        webView.scrollView.contentInsetAdjustmentBehavior = .automatic

        // 桥：回执与事件都走这个 WebView。
        bridge.webView = webView
        bridge.emitter = { [weak webView] event, payload in
            webView?.evaluateJavaScript(
                "window.__aicHostEmit(\(AicHostBridge.literal(event)), \(AicHostBridge.literal(payload)))"
            )
        }

        // cookie 同步：壳是唯一来源；刷新后这里替换，登出后这里清空。
        let store = webView.configuration.websiteDataStore.httpCookieStore
        auth.cookieSink = { [weak webView] cookies in
            guard let webView else { return }
            let store = webView.configuration.websiteDataStore.httpCookieStore
            if cookies.isEmpty {
                store.getAllCookies { existing in
                    for cookie in existing { store.delete(cookie, completionHandler: nil) }
                }
            } else {
                for cookie in cookies { store.setCookie(cookie) }
            }
        }

        // 先写 cookie 再加载：否则网页首屏请求不带会话。
        let cookies = auth.sessionCookies
        if cookies.isEmpty {
            webView.load(URLRequest(url: url))
        } else {
            let group = DispatchGroup()
            for cookie in cookies {
                group.enter()
                store.setCookie(cookie) { group.leave() }
            }
            group.notify(queue: .main) {
                webView.load(URLRequest(url: url))
            }
        }
        return webView
    }

    func updateUIView(_ webView: WKWebView, context: Context) {
        // 地址变了才重新加载，避免每次 SwiftUI 更新都刷新页面。
        if webView.url != url, context.coordinator.lastLoadedURL != url {
            context.coordinator.lastLoadedURL = url
            webView.load(URLRequest(url: url))
        }
    }

    static func dismantleUIView(_ webView: WKWebView, coordinator: Coordinator) {
        webView.configuration.userContentController.removeScriptMessageHandler(forName: AicHostBridge.messageName)
    }

    /// 注入脚本：把页面里的 JS 报错通过宿主桥回传（白屏时能直接看到原因）。
    private static let errorProbeScript = WKUserScript(source: """
    (function () {
      function report(message) {
        try {
          if (window.aicHost && typeof window.aicHost.call === 'function') {
            window.aicHost.call('page_error', { message: String(message) });
          }
        } catch (_) {}
      }
      window.addEventListener('error', function (e) {
        var where = (e.filename || '') + ':' + (e.lineno || '');
        report((e.message || 'script error') + (where ? ' @ ' + where : ''));
      });
      window.addEventListener('unhandledrejection', function (e) {
        var reason = e.reason;
        report('unhandled rejection: ' + (reason && reason.message ? reason.message : String(reason)));
      });
    })();
    """, injectionTime: .atDocumentStart, forMainFrameOnly: false)

    final class Coordinator: NSObject, WKNavigationDelegate {
        let bridge: AicHostBridge
        /// 平台域名：只有它留在壳内，站外链接交给系统（原生桥只注入受信任 origin）。
        let platformHost: String?
        let onLoad: () -> Void
        let onFail: (String) -> Void
        var lastLoadedURL: URL?

        init(bridge: AicHostBridge, platformHost: String?,
             onLoad: @escaping () -> Void = {}, onFail: @escaping (String) -> Void = { _ in }) {
            self.bridge = bridge
            self.platformHost = platformHost
            self.onLoad = onLoad
            self.onFail = onFail
        }

        func webView(_ webView: WKWebView, didFinish navigation: WKNavigation!) {
            onLoad()
        }

        func webView(_ webView: WKWebView, didFail navigation: WKNavigation!, withError error: Error) {
            onFail(error.localizedDescription)
        }

        func webView(_ webView: WKWebView, didFailProvisionalNavigation navigation: WKNavigation!, withError error: Error) {
            onFail(error.localizedDescription)
        }

        func webView(_ webView: WKWebView,
                     decidePolicyFor navigationAction: WKNavigationAction,
                     decisionHandler: @escaping (WKNavigationActionPolicy) -> Void) {
            guard let target = navigationAction.request.url else {
                decisionHandler(.allow)
                return
            }
            if target.scheme == "http" || target.scheme == "https" {
                if let platformHost, target.host == platformHost {
                    decisionHandler(.allow)
                } else {
                    decisionHandler(.cancel)
                    UIApplication.shared.open(target)
                }
            } else {
                decisionHandler(.cancel)
                UIApplication.shared.open(target)
            }
        }
    }
}
