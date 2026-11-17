//
//  ConsentView.swift
//  aic-pod · mobile/ios
//
//  首启同意弹层。协议链接用系统浏览器打开真实页面。
//

import SwiftUI

struct ConsentView: View {
    @StateObject private var model: ConsentViewModel
    @ObservedObject private var config: ServerConfig
    @State private var legalLink: LegalLink?
    /// 用户同意后的回调（进入工作区）。
    var onAgree: () -> Void

    init(config: ServerConfig, onAgree: @escaping () -> Void) {
        _model = StateObject(wrappedValue: ConsentViewModel(config: config))
        self.config = config
        self.onAgree = onAgree
    }

    var body: some View {
        ZStack {
            Color.black.opacity(0.42).ignoresSafeArea()

            VStack(spacing: 0) {
                Spacer(minLength: 0)
                card
                Spacer(minLength: 0)
            }
            .padding(.horizontal, 18)
        }
        .task { await model.load() }
        .sheet(item: $legalLink) { link in
            LegalDocumentSheet(url: link.url)
        }
    }

    private var card: some View {
        VStack(spacing: 0) {
            VStack(spacing: 10) {
                Text(config.t("consent.title", AppConfig.displayName))
                    .font(.system(size: 17, weight: .semibold))

                Text(config.t("consent.body"))
                    .font(.system(size: 13))
                    .foregroundStyle(.secondary)
                    .multilineTextAlignment(.leading)
                    .fixedSize(horizontal: false, vertical: true)

                HStack(spacing: 14) {
                    ForEach(model.documents) { document in
                        Button(document.title) {
                            if let url = document.absoluteURL(server: config.server) {
                                legalLink = LegalLink(url: url)
                            }
                        }
                        .font(.system(size: 13))
                        .foregroundStyle(Color.brandPrimary)
                        .underline(true, pattern: .solid)
                    }
                }

                if let notice = model.notice {
                    Text(notice)
                        .font(.system(size: 11))
                        .foregroundStyle(.secondary)
                }
            }
            .padding(.horizontal, 18)
            .padding(.top, 20)
            .padding(.bottom, 16)

            Button {
                model.agree()
                onAgree()
            } label: {
                Text(config.t("consent.agree"))
                    .font(.system(size: 15, weight: .semibold))
                    .frame(maxWidth: .infinity)
                    .padding(.vertical, 13)
                    .background(Brand.ink)
                    .foregroundStyle(Color.white)
            }
            .buttonStyle(.plain)
            .disabled(model.isLoading)

            Button {
                model.notice = config.t("consent.decline_notice", AppConfig.displayName)
            } label: {
                Text(config.t("consent.decline"))
                    .font(.system(size: 14))
                    .frame(maxWidth: .infinity)
                    .padding(.vertical, 11)
            }
            .buttonStyle(.plain)
            .foregroundStyle(Color.brandSecondary)
        }
        .background(Color.brandSurface)
        .clipShape(RoundedRectangle(cornerRadius: 16, style: .continuous))
        .shadow(color: .black.opacity(0.25), radius: 18, y: 8)
    }
}
