// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

// Package theme holds the single shared source of SNC visual-style CSS
// tokens (see shortnerdcat/VisualStyle.md §4.5/§4.7), used by both the
// macOS (WKWebView) and Linux (WebKitGTK) app windows. Both windows render
// near-identical HTML/CSS, so this is a single definition rather than two
// copies that could drift.
package theme

// CSS defines the SNC palette and typography as CSS custom properties.
// Callers prepend this to their own <style> block and reference tokens via
// var(--token-name) instead of hardcoded hex values.
const CSS = `:root{
  --bg-primary:#11141A;
  --surface:#1B2028;
  --kitten-black:#07090D;
  --fur-highlight:#252B34;
  --screen-cyan:#59C8FF;
  --warm-amber:#FFB547;
  --soft-lime:#A6D66D;
  --nerd-violet:#8B7CFF;
  --text-primary:#F4F1E8;
  --text-muted:#99A2B0;
  --font-ui:'Manrope',-apple-system,BlinkMacSystemFont,'Helvetica Neue',sans-serif;
  --font-mono:'JetBrains Mono',monospace;
}
`
