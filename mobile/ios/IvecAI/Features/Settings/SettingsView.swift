//
//  SettingsView.swift
//  aic-pod · mobile/ios
//
//  设置：壳里唯一有分量的原生页面。标题就叫「设置」。
//  账户分组：当前账户、退出登录、注销账户。
//

import SwiftUI
import SafariServices

struct SettingsView: View {
    @StateObject private var model: SettingsViewModel
    @ObservedObject private var config: ServerConfig
    @State private var showDeleteConfirm = false
    @State private var legalLink: LegalLink?
    /// 服务地址变更后的回调（工作区需要重载）。
    var onServerChanged: () -> Void

    init(model: SettingsViewModel, onServerChanged: @escaping () -> Void = {}) {
        _model = StateObject(wrappedValue: model)
        self.config = model.config
        self.onServerChanged = onServerChanged
    }

    var body: some View {
        NavigationStack {
            List {
                serviceSection
                appearanceSection
                accountSection
                privacySection
                aboutSection
            }
            .listStyle(.insetGrouped)
            .navigationTitle(config.t("settings.title"))
            .navigationBarTitleDisplayMode(.inline)
            .alert(config.t("delete.title"), isPresented: $showDeleteConfirm) {
                Button(config.t("settings.cancel"), role: .cancel) {}
                Button(config.t("settings.delete_account"), role: .destructive) {
                    Task { await model.deleteAccount() }
                }
            } message: {
                Text(config.t("delete.message"))
            }
            .alert(config.t("settings.operation_failed"), isPresented: Binding(
                get: { model.errorMessage != nil },
                set: { if !$0 { model.errorMessage = nil } }
            )) {
                Button(config.t("common.ok"), role: .cancel) { model.errorMessage = nil }
            } message: {
                Text(model.errorMessage ?? "")
            }
            .sheet(item: $legalLink) { link in
                LegalDocumentSheet(url: link.url)
            }
            .task {
                await model.loadLegalLinks()
            }
        }
    }

    private var serviceSection: some View {
        Section(config.t("settings.server")) {
            HStack {
                Text(config.t("settings.server_address"))
                Spacer()
                TextField("https://ivec-ai.com", text: $model.serverText)
                    .multilineTextAlignment(.trailing)
                    .autocorrectionDisabled()
                    .textInputAutocapitalization(.never)
                    .keyboardType(.URL)
                    .foregroundStyle(.secondary)
                    .frame(maxWidth: 190)
            }
            Button(config.t("settings.save_reconnect")) {
                model.saveServer()
                onServerChanged()
            }
            .disabled(!model.serverIsValid)

            LabeledContent(config.t("settings.connection")) {
                Text(model.isSignedIn ? config.t("settings.connected") : config.t("settings.not_signed_in"))
                    .foregroundStyle(model.isSignedIn ? Color.green : .secondary)
            }
        }
    }

    private var appearanceSection: some View {
        Section(config.t("settings.appearance")) {
            Picker(config.t("settings.theme"), selection: Binding(
                get: { model.theme },
                set: { model.saveTheme($0) }
            )) {
                ForEach(AppTheme.allCases) { theme in
                    Text(config.t(theme.titleKey)).tag(theme)
                }
            }
            Picker(config.t("settings.language"), selection: Binding(
                get: { model.language },
                set: { model.saveLanguage($0) }
            )) {
                ForEach(AppLanguage.allCases) { language in
                    Text(config.t(language.titleKey)).tag(language)
                }
            }
        }
    }

    private var accountSection: some View {
        Section {
            LabeledContent(config.t("settings.signed_in")) {
                Text(model.isSignedIn ? model.currentUserName : config.t("settings.not_signed_in"))
                    .foregroundStyle(.secondary)
            }
            if model.isSignedIn {
                Button(config.t("settings.sign_out")) {
                    Task { await model.signOut() }
                }
            }
            Button(role: .destructive) {
                showDeleteConfirm = true
            } label: {
                HStack {
                    Text(config.t("settings.delete_account"))
                    Spacer()
                    if model.isDeleting {
                        ProgressView().controlSize(.small)
                    }
                }
            }
            .disabled(model.isDeleting)
        } header: {
            Text(config.t("settings.account"))
        } footer: {
            Text(config.t("settings.delete_account_note"))
        }
    }

    private var privacySection: some View {
        Section(config.t("settings.privacy")) {
            if let privacy = model.privacyURL {
                Button(config.t("settings.privacy_policy")) {
                    legalLink = LegalLink(url: model.config.server.sitePath(privacy))
                }
            }
            if let terms = model.termsURL {
                Button(config.t("settings.terms")) {
                    legalLink = LegalLink(url: model.config.server.sitePath(terms))
                }
            }
        }
    }

    private var aboutSection: some View {
        Section(config.t("settings.about")) {
            LabeledContent(config.t("settings.version"), value: Bundle.main.shortVersion)
            LabeledContent(config.t("settings.license"), value: "MIT")
            Link(config.t("settings.open_source"), destination: URL(string: "https://github.com/veypi/aic-pod")!)
        }
    }
}

extension Bundle {
    var shortVersion: String {
        let version = infoDictionary?["CFBundleShortVersionString"] as? String ?? "0.1.0"
        return "\(version) (\(buildNumber))"
    }

    /// 构建号（aicHost 的 app_info 用）。
    var buildNumber: String {
        infoDictionary?["CFBundleVersion"] as? String ?? "1"
    }
}
