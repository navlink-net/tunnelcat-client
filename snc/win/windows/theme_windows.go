// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build windows

package windows

// SNC visual style palette (see shortnerdcat/VisualStyle.md §4.5) as Win32
// COLORREF values. COLORREF packs as R | G<<8 | B<<16 (see the comment on
// uiClrBg in uiwindow.go) -- NOT the RGB hex byte order, so these constants
// are computed from the hex triples rather than pasted directly.
const (
	ThemeBgPrimary    uint32 = 0x11 | (0x14 << 8) | (0x1A << 16) // #11141A
	ThemeSurface      uint32 = 0x1B | (0x20 << 8) | (0x28 << 16) // #1B2028
	ThemeKittenBlack  uint32 = 0x07 | (0x09 << 8) | (0x0D << 16) // #07090D
	ThemeFurHighlight uint32 = 0x25 | (0x2B << 8) | (0x34 << 16) // #252B34
	ThemeScreenCyan   uint32 = 0x59 | (0xC8 << 8) | (0xFF << 16) // #59C8FF
	ThemeWarmAmber    uint32 = 0xFF | (0xB5 << 8) | (0x47 << 16) // #FFB547
	ThemeSoftLime     uint32 = 0xA6 | (0xD6 << 8) | (0x6D << 16) // #A6D66D
	ThemeNerdViolet   uint32 = 0x8B | (0x7C << 8) | (0xFF << 16) // #8B7CFF
	ThemeTextPrimary  uint32 = 0xF4 | (0xF1 << 8) | (0xE8 << 16) // #F4F1E8
	ThemeTextMuted    uint32 = 0x99 | (0xA2 << 8) | (0xB0 << 16) // #99A2B0
)

// Typeface names per VisualStyle.md §4.7. GDI's CreateFontW silently falls
// back to a default system font if the named face isn't installed, so this
// is safe to reference even before Manrope/JetBrains Mono .ttf files are
// sourced and installed/embedded (tracked separately -- font embedding is a
// real asset-sourcing task, not pure code).
const (
	ThemeFontUI   = "Manrope"
	ThemeFontMono = "JetBrains Mono"
)
