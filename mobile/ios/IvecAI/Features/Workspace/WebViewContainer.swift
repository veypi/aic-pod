//
//  WebViewContainer.swift
//  aic-pod · mobile/ios
//
//  WKWebView 封装：加载平台工作区，接收页面消息，转交外部链接。
//

import SwiftUI
import WebKit

struct WebViewContainer: UIViewRepresentable {
    let url: URL
    /// 页面 → 壳的消息。
    var onMessage: (ShellInboundMessage) -> Void
    /// 页面加载完成（遮罩应撤掉）。
    var onLoad: () -> Void = {}
    /// 页面加载失败。
    var onFail: (String) -> Void = { _ in }
    /// 壳 → 页面。
    var postToPage: String?
    /// 登录会话 cookie：写入 webview 的 cookie 存储，网页即认这个登录态。
    var cookies: [HTTPCookie] = []

    func makeCoordinator() -> Coordinator {
        Coordinator(onMessage: onMessage, onLoad: onLoad, onFail: onFail)
    }

    func makeUIView(context: Context) -> WKWebView {
        let config = WKWebViewConfiguration()
        config.websiteDataStore = .default()
        config.defaultWebpagePreferences.allowsContentJavaScript = true

        let webView = WKWebView(frame: .zero, configuration: config)
        webView.navigationDelegate = context.coordinator
        webView.allowsBackForwardNavigationGestures = true
        webView.scrollView.contentInsetAdjustmentBehavior = .automatic

        let controller = WKUserContentController()
        controller.add(context.coordinator, name: ShellMessageName.handler)
        controller.addUserScript(Self.errorProbeScript)
        webView.configuration.userContentController = controller

        // 先写 cookie 再加载：否则网页首屏请求不带会话。
        if cookies.isEmpty {
            webView.load(URLRequest(url: url))
        } else {
            let group = DispatchGroup()
            for cookie in cookies {
                group.enter()
                webView.configuration.websiteDataStore.httpCookieStore.setCookie(cookie) { group.leave() }
            }
            group.notify(queue: .main) {
                webView.load(URLRequest(url: url))
            }
        }
        return webView
    }
    func updateUIView(_ webView: WKWebView, context: Context) {
        // 壳 → 页面的消息：只在内容变化时下发。
        if let postToPage, postToPage != context.coordinator.lastPosted {
            context.coordinator.lastPosted = postToPage
            webView.evaluateJavaScript("window.dispatchEvent(new MessageEvent('ivec', {data: \(postToPage)}))")
        }
        // 地址变了才重新加载，避免每次 SwiftUI 更新都刷新页面。
        if webView.url != url, context.coordinator.lastLoadedURL != url {
            context.coordinator.lastLoadedURL = url
            webView.load(URLRequest(url: url))
        }
    }

    static func dismantleUIView(_ webView: WKWebView, coordinator: Coordinator) {
        webView.configuration.userContentController.removeScriptMessageHandler(forName: ShellMessageName.handler)
    }

    /// 注入脚本：把页面里的 JS 报错回传给壳（白屏时能直接看到原因）。
    private static let errorProbeScript = WKUserScript(source: """
    (function () {
      function send(message) {
        try {
          window.webkit.messageHandlers.ivec.postMessage({ type: 'page.error', message: String(message) });
        } catch (_) {}
      }
      window.addEventListener('error', function (e) {
        var where = (e.filename || '') + ':' + (e.lineno || '');
        send((e.message || 'script error') + (where ? ' @ ' + where : ''));
      });
      window.addEventListener('unhandledrejection', function (e) {
        var reason = e.reason;
        send('unhandled rejection: ' + (reason && reason.message ? reason.message : String(reason)));
      });
      var originalError = console.error;
      console.error = function () {
        try { send(Array.prototype.map.call(arguments, String).join(' ')); } catch (_) {}
        originalError.apply(console, arguments);
      };
    })();
    """, injectionTime: .atDocumentStart, forMainFrameOnly: false)

    final class Coordinator: NSObject, WKNavigationDelegate, WKScriptMessageHandler {
        let onMessage: (ShellInboundMessage) -> Void
        let onLoad: () -> Void
        let onFail: (String) -> Void
        var lastPosted: String?
        var lastLoadedURL: URL?

        init(onMessage: @escaping (ShellInboundMessage) -> Void,
             onLoad: @escaping () -> Void = {},
             onFail: @escaping (String) -> Void = { _ in }) {
            self.onMessage = onMessage
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

        func userContentController(_ userContentController: WKUserContentController,
                                   didReceive message: WKScriptMessage) {
            guard message.name == ShellMessageName.handler else { return }
            guard let inbound = ShellInboundMessage(payload: message.body) else { return }
            onMessage(inbound)
        }

        func webView(_ webView: WKWebView,
                     decidePolicyFor navigationAction: WKNavigationAction,
                     decisionHandler: @escaping (WKNavigationActionPolicy) -> Void) {
            guard let target = navigationAction.request.url else {
                decisionHandler(.allow)
                return
            }
            // http(s) 留在壳内；其它协议（tel/mailto 等）交给系统。
            if target.scheme == "http" || target.scheme == "https" {
                decisionHandler(.allow)
            } else {
                decisionHandler(.cancel)
                UIApplication.shared.open(target)
            }
        }
    }
}
