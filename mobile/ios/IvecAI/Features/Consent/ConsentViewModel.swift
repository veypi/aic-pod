//
//  ConsentViewModel.swift
//  aic-pod · mobile/ios
//
//  首启协议同意：清单来自后台设置 auth.legal_documents，
//  同意后把时间与清单落到本机（合规留痕的本地一半）。
//

import Foundation
import SwiftUI

@MainActor
final class ConsentViewModel: ObservableObject {
    @Published private(set) var documents: [LegalDocument] = []
    @Published private(set) var isLoading = true
    @Published var notice: String?

    let config: ServerConfig

    init(config: ServerConfig) {
        self.config = config
    }

    /// 拉取协议清单；失败时不阻塞使用（降级为无协议清单）。
    func load() async {
        isLoading = true
        defer { isLoading = false }
        do {
            documents = try await LegalDocuments.fetch(server: config.server)
        } catch {
            documents = []
            notice = config.t("consent.load_failed")
        }
    }

    /// 记录同意：时间 + 当时的清单。
    func agree() {
        config.consent = ServerConfig.ConsentRecord(
            agreedAt: Date(),
            documents: documents
        )
    }

    /// 是否已经同意过（同意后不再重复打扰）。
    var hasConsented: Bool {
        config.consent != nil
    }
}
