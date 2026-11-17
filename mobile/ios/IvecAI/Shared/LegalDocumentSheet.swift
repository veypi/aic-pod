//
//  LegalDocumentSheet.swift
//  aic-pod · mobile/ios
//
//  协议页用系统 Safari 视图打开（真实系统能力，不是网页里的假链接）。
//

import SwiftUI
import SafariServices

struct LegalDocumentSheet: UIViewControllerRepresentable {
    let url: URL

    func makeUIViewController(context: Context) -> SFSafariViewController {
        let controller = SFSafariViewController(url: url)
        controller.preferredControlTintColor = UIColor(Brand.ink)
        return controller
    }

    func updateUIViewController(_ controller: SFSafariViewController, context: Context) {}
}
