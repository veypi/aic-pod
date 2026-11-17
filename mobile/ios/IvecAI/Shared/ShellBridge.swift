//
//  ShellBridge.swift
//  aic-pod · mobile/ios
//
//  网页与原生壳之间唯一的通道。页面只做两件事：
//  把登录令牌交给壳（壳存 Keychain，供注销接口用）、接收壳的指令。
//

import Foundation
import WebKit

/// 消息名（网页侧：window.webkit.messageHandlers.ivec.postMessage(...)）。
enum ShellMessageName {
    static let handler = "ivec"
}

/// 页面 → 壳的消息。
enum ShellInboundMessage {
    /// 登录成功后的 auth 令牌。
    case authToken(String)
    /// 用户在页面内主动退出登录。
    case signedOut
    /// 页面里的 JS 报错（白屏时直接在壳上显示原因）。
    case pageError(String)

    init?(payload: Any) {
        guard let dict = payload as? [String: Any],
              let type = dict["type"] as? String else { return nil }
        switch type {
        case "auth.token":
            guard let token = dict["token"] as? String else { return nil }
            self = .authToken(token)
        case "auth.signedOut":
            self = .signedOut
        case "page.error":
            guard let message = dict["message"] as? String else { return nil }
            self = .pageError(message)
        default:
            return nil
        }
    }
}

/// 壳 → 页面的消息（网页侧监听 window.addEventListener('ivec', ...)）。
enum ShellOutboundMessage {
    /// 账号已注销：页面应清掉本地登录态并回到登录页。
    case accountDeleted
    /// 服务端地址变更：页面需要重新加载。
    case serverChanged(String)

    var json: String {
        switch self {
        case .accountDeleted:
            return #"{"type":"account.deleted"}"#
        case let .serverChanged(url):
            let escaped = url.replacingOccurrences(of: "\\", with: "\\\\").replacingOccurrences(of: "\"", with: "\\\"")
            return "{\"type\":\"server.changed\",\"server\":\"\(escaped)\"}"
        }
    }
}
