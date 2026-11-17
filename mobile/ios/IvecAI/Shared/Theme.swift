//
//  Theme.swift
//  aic-pod · mobile/ios
//
//  品牌视觉：ivec 是单色系（纯黑标记 + #f1f8ff 浅底），
//  深色模式按品牌规则反相。壳侧不引入额外彩色。
//

import SwiftUI

enum Brand {
    /// 品牌浅底（同时是图标底色）。
    static let field = Color(red: 241 / 255, green: 248 / 255, blue: 255 / 255)

    /// 主色（浅色模式下的品牌黑）。
    static let ink = Color(red: 17 / 255, green: 24 / 255, blue: 39 / 255)
}

extension Color {
    /// 语义色：跟随系统浅/深色。
    static var brandBackground: Color {
        Color(uiColor: .systemGroupedBackground)
    }

    static var brandSurface: Color {
        Color(uiColor: .secondarySystemGroupedBackground)
    }

    static var brandPrimary: Color {
        Color(uiColor: .label)
    }

    static var brandSecondary: Color {
        Color(uiColor: .secondaryLabel)
    }
}

/// 主题应用：把 ServerConfig 的选择映射成 SwiftUI 配色方案。
struct BrandThemeModifier: ViewModifier {
    @Environment(\.colorScheme) private var systemScheme
    let theme: AppTheme

    func body(content: Content) -> some View {
        content
            .preferredColorScheme(scheme)
            .tint(Brand.ink)
    }

    private var scheme: ColorScheme? {
        switch theme {
        case .system: return nil
        case .light: return .light
        case .dark: return .dark
        }
    }
}

extension View {
    func brandTheme(_ theme: AppTheme) -> some View {
        modifier(BrandThemeModifier(theme: theme))
    }
}
