// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

import UIKit

/// SNC visual style palette and typography (shortnerdcat/VisualStyle.md §4.5,
/// §4.7). Manrope/JetBrains Mono are not guaranteed to be bundled into the
/// app target yet (that's separate font-asset-sourcing work) — the Font
/// helpers below fall back to system fonts if the named font isn't
/// registered, so referencing them is always safe.
enum SNCTheme {
    static let bgPrimary    = UIColor(red: 0x11 / 255, green: 0x14 / 255, blue: 0x1A / 255, alpha: 1)
    static let surface      = UIColor(red: 0x1B / 255, green: 0x20 / 255, blue: 0x28 / 255, alpha: 1)
    static let kittenBlack  = UIColor(red: 0x07 / 255, green: 0x09 / 255, blue: 0x0D / 255, alpha: 1)
    static let furHighlight = UIColor(red: 0x25 / 255, green: 0x2B / 255, blue: 0x34 / 255, alpha: 1)
    static let screenCyan   = UIColor(red: 0x59 / 255, green: 0xC8 / 255, blue: 0xFF / 255, alpha: 1)
    static let warmAmber    = UIColor(red: 0xFF / 255, green: 0xB5 / 255, blue: 0x47 / 255, alpha: 1)
    static let softLime     = UIColor(red: 0xA6 / 255, green: 0xD6 / 255, blue: 0x6D / 255, alpha: 1)
    static let nerdViolet   = UIColor(red: 0x8B / 255, green: 0x7C / 255, blue: 0xFF / 255, alpha: 1)
    static let textPrimary  = UIColor(red: 0xF4 / 255, green: 0xF1 / 255, blue: 0xE8 / 255, alpha: 1)
    static let textMuted    = UIColor(red: 0x99 / 255, green: 0xA2 / 255, blue: 0xB0 / 255, alpha: 1)

    enum Font {
        static func extraBold(_ size: CGFloat) -> UIFont {
            UIFont(name: "Manrope-ExtraBold", size: size) ?? .systemFont(ofSize: size, weight: .heavy)
        }
        static func bold(_ size: CGFloat) -> UIFont {
            UIFont(name: "Manrope-Bold", size: size) ?? .systemFont(ofSize: size, weight: .bold)
        }
        static func medium(_ size: CGFloat) -> UIFont {
            UIFont(name: "Manrope-Medium", size: size) ?? .systemFont(ofSize: size, weight: .medium)
        }
        static func regular(_ size: CGFloat) -> UIFont {
            UIFont(name: "Manrope-Regular", size: size) ?? .systemFont(ofSize: size, weight: .regular)
        }
        static func mono(_ size: CGFloat) -> UIFont {
            UIFont(name: "JetBrainsMono-Medium", size: size) ?? .monospacedSystemFont(ofSize: size, weight: .medium)
        }
    }
}
