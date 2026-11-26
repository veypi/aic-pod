//
//  AicHostBridge.swift
//  aic-pod · mobile/ios
//
//  网页与原生壳之间的统一桥：window.aicHost。
//  协议（能力、方法、事件）见 aic-pod/docs/development/client-bridge.md §七。
//  旧的 ShellBridge 消息（auth.token / auth.signedOut / page.error）不再保留。
//

import Foundation
import WebKit

final class AicHostBridge: NSObject, WKScriptMessageHandler {
    /// 消息通道名（页面侧 window.webkit.messageHandlers.aicHost）。
    static let messageName = "aicHost"

    /// 页面方法 → 宿主实现；返回字典即成功，抛错即失败（不静默回退 Web）。
    var handler: ((String, [String: Any]) async throws -> [String: Any])?
    /// 宿主 → 页面事件。
    var emitter: ((String, [String: Any]) -> Void)?

    /// 承载页面的 WebView（回执与事件都走它）。
    weak var webView: WKWebView?

    /// 注入页面的桥脚本（documentStart 执行）。
    static let script = """
    (function () {
      if (window.aicHost && window.aicHost.version === 1) return;
      var pending = {};
      var listeners = {};
      var seq = 0;

      function post(method, params) {
        return new Promise(function (resolve, reject) {
          var id = 'c' + (++seq);
          pending[id] = { resolve: resolve, reject: reject };
          try {
            window.webkit.messageHandlers.aicHost.postMessage({ id: id, method: method, params: params || {} });
          } catch (error) {
            delete pending[id];
            reject(new Error(String((error && error.message) || error)));
          }
        });
      }

      window.__aicHostResolve = function (id, result) {
        var entry = pending[id];
        if (!entry) return;
        delete pending[id];
        entry.resolve(result);
      };

      window.__aicHostReject = function (id, message) {
        var entry = pending[id];
        if (!entry) return;
        delete pending[id];
        entry.reject(new Error(message || 'aicHost call failed'));
      };

      window.__aicHostEmit = function (event, payload) {
        var list = listeners[event];
        if (!list) return;
        list.slice().forEach(function (handler) {
          try { handler(payload); } catch (error) { console.error('aicHost handler failed', error); }
        });
      };

      window.aicHost = {
        version: 1,
        platform: 'ios',
        capabilities: {
          auth: true,
          accountDelete: true,
          share: false,
          clipboard: false,
          files: false,
          notify: false,
          external: false,
          deeplink: false,
          theme: false,
          locale: false
        },
        call: function (method, params) { return post(method, params); },
        on: function (event, handler) {
          if (typeof handler !== 'function') throw new TypeError('aicHost event handler must be a function');
          var list = listeners[event] || (listeners[event] = []);
          list.push(handler);
          return function () {
            var current = listeners[event] || [];
            var index = current.indexOf(handler);
            if (index >= 0) current.splice(index, 1);
          };
        },
        off: function (event, handler) {
          var current = listeners[event] || [];
          var index = current.indexOf(handler);
          if (index >= 0) current.splice(index, 1);
        }
      };
    })();
    """

    func userContentController(_ userContentController: WKUserContentController,
                               didReceive message: WKScriptMessage) {
        guard message.name == AicHostBridge.messageName else { return }
        guard let body = message.body as? [String: Any],
              let id = body["id"] as? String,
              let method = body["method"] as? String else { return }
        let params = body["params"] as? [String: Any] ?? [:]
        Task { @MainActor [weak self] in
            guard let self else { return }
            do {
                let result = try await self.handler?(method, params) ?? [:]
                self.reply(id: id, result: result)
            } catch {
                self.reply(id: id, error: error.localizedDescription)
            }
        }
    }

    /// 宿主 → 页面事件（如 auth.changed）。
    func emit(_ event: String, _ payload: [String: Any]) {
        emitter?(event, payload)
    }

    private func reply(id: String, result: [String: Any]) {
        evaluate("window.__aicHostResolve(\(AicHostBridge.literal(id)), \(AicHostBridge.literal(result)))")
    }

    private func reply(id: String, error message: String) {
        evaluate("window.__aicHostReject(\(AicHostBridge.literal(id)), \(AicHostBridge.literal(message)))")
    }

    private func evaluate(_ script: String) {
        webView?.evaluateJavaScript(script) { _, error in
            if let error {
                AicLog.debug("aicHost evaluate failed: \(String(describing: error))")
            }
        }
    }

    /// JSON 字面量（JSON 是合法 JS 字面量；U+2028/2029 需转义）。
    static func literal(_ value: Any) -> String {
        guard let data = try? JSONSerialization.data(withJSONObject: value, options: [.fragmentsAllowed]),
              let text = String(data: data, encoding: .utf8) else {
            return "null"
        }
        return text
            .replacingOccurrences(of: "\u{2028}", with: "\\u2028")
            .replacingOccurrences(of: "\u{2029}", with: "\\u2029")
    }
}
