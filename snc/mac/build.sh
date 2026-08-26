#!/bin/bash
# build.sh — builds ShortNerdCat.app for macOS (universal binary) and packages a DMG.
#
# Usage:
#   ./build.sh [--skip-notarize] [--version <version>]
#
# Prerequisites (Mac):
#   - Go 1.21+
#   - Xcode Command Line Tools (for codesign, xcrun, hdiutil, lipo, sips, iconutil)
#   - Developer ID Application: Konstantin Khait (AF6BSD27T9) in Keychain
#   - xcrun notarytool keychain profile named "notarization-profile"
#     (set up once with: xcrun notarytool store-credentials notarization-profile
#                         --apple-id YOUR_APPLE_ID --team-id AF6BSD27T9)
#
# Environment variables (override defaults):
#   CERT_ID             codesign certificate name (default below)
#   APPLE_TEAM_ID       Team ID for notarization (default AF6BSD27T9)
#   KEYCHAIN_PROFILE    xcrun notarytool profile name (default notarization-profile)

set -euo pipefail

# ── Find Go ───────────────────────────────────────────────────────────────────

for _godir in \
    "$(go env GOROOT 2>/dev/null)/bin" \
    "$HOME/go-sdk/bin" \
    "$HOME/sdk/go/bin" \
    /usr/local/go/bin \
    /opt/homebrew/bin \
    /usr/local/bin
do
    if [[ -x "$_godir/go" ]]; then
        export PATH="$_godir:$PATH"
        break
    fi
done

command -v go >/dev/null 2>&1 || { echo "ERROR: go not found"; exit 1; }

# ── Configuration ─────────────────────────────────────────────────────────────

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

APP_NAME="ShortNerdCat"
BUNDLE_ID="net.navlink.shortnerdcat"
BINARY_NAME="shortnerdcat"

CERT_ID="${CERT_ID:-Developer ID Application: Konstantin Khait (AF6BSD27T9)}"
APPLE_TEAM_ID="${APPLE_TEAM_ID:-AF6BSD27T9}"
KEYCHAIN_PROFILE="${KEYCHAIN_PROFILE:-notarization-profile}"

SKIP_NOTARIZE=false
SKIP_SIGN=false
VERSION=""

# Parse arguments
while [[ $# -gt 0 ]]; do
    case "$1" in
        --skip-notarize) SKIP_NOTARIZE=true; shift ;;
        --skip-sign)     SKIP_SIGN=true; SKIP_NOTARIZE=true; shift ;;
        --version) VERSION="$2"; shift 2 ;;
        *) echo "Unknown argument: $1"; exit 1 ;;
    esac
done

# Version stamp: YYYYMMDDHHmm — mirrors Windows build.bat behaviour.
if [[ -z "$VERSION" ]]; then
    VERSION="$(date +%Y%m%d%H%M)"
fi

echo "Building $APP_NAME $VERSION..."

# ── Paths ─────────────────────────────────────────────────────────────────────

BUNDLE_SRC="$SCRIPT_DIR/bundle"
ASSETS_DST="$SCRIPT_DIR/macos/assets"
ICON_SRC="$REPO_ROOT/snc/win/windows/assets"

# BUILD_DIR must match deploy.sh's BUILD_DIR exactly. They used to differ
# (deploy.sh looked in snc/mac/build/ instead of here), which let a stale
# DMG committed at that other path get silently re-uploaded with a fresh
# version stamp on every deploy for months -- see the 2026-08-10 incident:
# a downloaded DMG's embedded Info.plist still read a build from months
# earlier despite the reported OTA version being current. If you change
# this path, change deploy.sh's BUILD_DIR to match.
BUILD_DIR="$REPO_ROOT/build/mac"
APP_DIR="$BUILD_DIR/$APP_NAME.app"
MACOS_DIR="$APP_DIR/Contents/MacOS"
RESOURCES_DIR="$APP_DIR/Contents/Resources"

DMG_STAGING="$BUILD_DIR/dmg-staging"
DMG_TMP="$BUILD_DIR/${APP_NAME}-temp.dmg"
DMG_FINAL="$BUILD_DIR/${APP_NAME}.dmg"

ICON_TMP="$BUILD_DIR/App.iconset"
BG_IMG="$ICON_SRC/bg.png"

# ── Step 1: Copy icon PNGs into the embed assets directory ───────────────────

echo "Copying tray icon assets..."
mkdir -p "$ASSETS_DST"
for f in snc_idle.png snc_connecting.png snc_connected.png snc_error.png snc_vless.png snc_wildcat.png bg.png logo.png; do
    cp "$ICON_SRC/$f" "$ASSETS_DST/$f"
done

# ── Step 2: Build amd64 and arm64 binaries ────────────────────────────────────

echo "Building amd64..."
CGO_ENABLED=1 GOARCH=amd64 GOOS=darwin \
    CC="clang -arch x86_64" \
    CXX="clang++ -arch x86_64" \
    go build -C "$REPO_ROOT" \
    -ldflags "-X tunnel_cat/snc/core.Version=$VERSION" \
    -o "$BUILD_DIR/${BINARY_NAME}-amd64" \
    ./snc/mac/cmd/shortnerdcat

echo "Building arm64..."
CGO_ENABLED=1 GOARCH=arm64 GOOS=darwin \
    CC="clang -arch arm64" \
    CXX="clang++ -arch arm64" \
    go build -C "$REPO_ROOT" \
    -ldflags "-X tunnel_cat/snc/core.Version=$VERSION" \
    -o "$BUILD_DIR/${BINARY_NAME}-arm64" \
    ./snc/mac/cmd/shortnerdcat

echo "Creating universal binary..."
lipo -create -output "$BUILD_DIR/$BINARY_NAME" \
    "$BUILD_DIR/${BINARY_NAME}-amd64" \
    "$BUILD_DIR/${BINARY_NAME}-arm64"
rm "$BUILD_DIR/${BINARY_NAME}-amd64" "$BUILD_DIR/${BINARY_NAME}-arm64"

# ── Step 3: Assemble .app bundle ──────────────────────────────────────────────

echo "Assembling $APP_NAME.app..."
rm -rf "$APP_DIR"
mkdir -p "$MACOS_DIR" "$RESOURCES_DIR"

# Binary
cp "$BUILD_DIR/$BINARY_NAME" "$MACOS_DIR/$BINARY_NAME"
chmod 755 "$MACOS_DIR/$BINARY_NAME"

# Launcher script (CFBundleExecutable)
cp "$BUNDLE_SRC/ShortNerdCat.sh" "$MACOS_DIR/ShortNerdCat.sh"
chmod 755 "$MACOS_DIR/ShortNerdCat.sh"

# Info.plist (inject version)
cp "$BUNDLE_SRC/Info.plist" "$APP_DIR/Contents/Info.plist"
/usr/libexec/PlistBuddy -c "Set :CFBundleVersion $VERSION" "$APP_DIR/Contents/Info.plist"
/usr/libexec/PlistBuddy -c "Set :CFBundleShortVersionString $VERSION" "$APP_DIR/Contents/Info.plist"

# App icon — logo.png is required; Info.plist already references AppIcon.icns.
APP_ICON_PNG="$ICON_SRC/logo.png"
[[ -f "$APP_ICON_PNG" ]] || { echo "ERROR: $APP_ICON_PNG not found — cannot build icon"; exit 1; }
mkdir -p "$ICON_TMP"
sips -z 16   16   "$APP_ICON_PNG" --out "$ICON_TMP/icon_16x16.png"       >/dev/null
sips -z 32   32   "$APP_ICON_PNG" --out "$ICON_TMP/icon_16x16@2x.png"    >/dev/null
sips -z 32   32   "$APP_ICON_PNG" --out "$ICON_TMP/icon_32x32.png"       >/dev/null
sips -z 64   64   "$APP_ICON_PNG" --out "$ICON_TMP/icon_32x32@2x.png"    >/dev/null
sips -z 128  128  "$APP_ICON_PNG" --out "$ICON_TMP/icon_128x128.png"     >/dev/null
sips -z 256  256  "$APP_ICON_PNG" --out "$ICON_TMP/icon_128x128@2x.png"  >/dev/null
sips -z 256  256  "$APP_ICON_PNG" --out "$ICON_TMP/icon_256x256.png"     >/dev/null
sips -z 512  512  "$APP_ICON_PNG" --out "$ICON_TMP/icon_256x256@2x.png"  >/dev/null
sips -z 512  512  "$APP_ICON_PNG" --out "$ICON_TMP/icon_512x512.png"     >/dev/null
sips -z 1024 1024 "$APP_ICON_PNG" --out "$ICON_TMP/icon_512x512@2x.png"  >/dev/null
iconutil -c icns "$ICON_TMP" -o "$RESOURCES_DIR/AppIcon.icns"
rm -rf "$ICON_TMP"

# ── Step 4: Sign the bundle ───────────────────────────────────────────────────

if [[ "$SKIP_SIGN" == "true" ]]; then
    echo "Ad-hoc signing (--skip-sign)..."
    codesign --force --deep --sign - "$APP_DIR"
else
    echo "Signing embedded Mach-O binaries..."
    find "$APP_DIR" -type f | while read -r f; do
        if file "$f" 2>/dev/null | grep -q "Mach-O"; then
            codesign --force --timestamp --sign "$CERT_ID" "$f" 2>/dev/null || true
        fi
    done

    echo "Signing binary (with entitlements + Hardened Runtime)..."
    codesign --force --timestamp --options runtime \
        --entitlements "$BUNDLE_SRC/entitlements.plist" \
        --sign "$CERT_ID" \
        "$MACOS_DIR/$BINARY_NAME"

    echo "Deep-signing app bundle..."
    codesign --deep --force --timestamp --options runtime \
        --entitlements "$BUNDLE_SRC/entitlements.plist" \
        --sign "$CERT_ID" \
        "$APP_DIR"

    echo "Verifying signature..."
    codesign --verify --deep --strict "$APP_DIR"
    spctl --assess --type exec "$APP_DIR" || echo "Note: spctl may report 'not notarized yet' at this stage."
fi

# ── Step 5: Package DMG ───────────────────────────────────────────────────────

echo "Creating DMG..."
APP_SIZE_KB=$(du -sk "$APP_DIR" | awk '{print $1}')
DMG_SIZE_MB=$((APP_SIZE_KB / 1024 + 150))

rm -rf "$DMG_STAGING" "$DMG_TMP" "$DMG_FINAL"
mkdir -p "$DMG_STAGING"
cp -R "$APP_DIR" "$DMG_STAGING/"
ln -s /Applications "$DMG_STAGING/Applications"

hdiutil create \
    -size "${DMG_SIZE_MB}m" \
    -fs HFS+ \
    -volname "$APP_NAME" \
    -srcfolder "$DMG_STAGING" \
    -ov -format UDRW \
    "$DMG_TMP" >/dev/null

echo "Configuring DMG window layout..."
ATTACH_OUTPUT=$(hdiutil attach -readwrite -noverify -noautoopen "$DMG_TMP")
DEVICE=$(echo "$ATTACH_OUTPUT" | awk '$3 ~ /^\/Volumes\// { print $1; exit }')
MOUNT_DIR=$(echo "$ATTACH_OUTPUT" | awk '$3 ~ /^\/Volumes\// { for(i=3;i<=NF;i++) printf $i " "; print ""; exit }' | sed 's/ *$//')

# Generate a light-gradient DMG background (top: steel-blue #dce6f5, bottom: near-white #f5f8ff)
# so black icon labels are legible.  Window inner content area is 560×340.
DMG_BG="$BUILD_DIR/dmg-bg.png"
python3 - "$DMG_BG" <<'PYEOF'
import sys, struct, zlib

def chunk(tag, data):
    crc = zlib.crc32(tag + data) & 0xffffffff
    return struct.pack('>I', len(data)) + tag + data + struct.pack('>I', crc)

w, h = 560, 340
rows = []
for y in range(h):
    t = y / max(h - 1, 1)
    r = int(220 + (245 - 220) * t)
    g = int(230 + (248 - 230) * t)
    b = int(245 + (255 - 245) * t)
    rows.append(bytes([0] + [r, g, b] * w))

compressed = zlib.compress(b''.join(rows), 9)
out  = b'\x89PNG\r\n\x1a\n'
out += chunk(b'IHDR', struct.pack('>IIBBBBB', w, h, 8, 2, 0, 0, 0))
out += chunk(b'IDAT', compressed)
out += chunk(b'IEND', b'')
with open(sys.argv[1], 'wb') as f:
    f.write(out)
PYEOF

mkdir -p "${MOUNT_DIR}/.background"
cp -f "$DMG_BG" "${MOUNT_DIR}/.background/bg.png"

# Set the DMG volume icon: copy AppIcon.icns as the hidden .VolumeIcon.icns
# and set the kHasCustomIcon bit (0x0400) in the volume root's Finder metadata.
cp "$RESOURCES_DIR/AppIcon.icns" "${MOUNT_DIR}/.VolumeIcon.icns"
xattr -wx com.apple.FinderInfo \
    "0000000000000000040000000000000000000000000000000000000000000000" \
    "${MOUNT_DIR}"

# Window: 560×360 outer → content 560×338.
# Icons (128 px) centered: x midpoint=280 → app at x=140, Applications at x=420.
# Vertical: y=165 places icon bodies (±64 px) at rows 101–229, labels at ~235.
osascript <<APPLESCRIPT
tell application "Finder"
    tell disk "${APP_NAME}"
        open
        set current view of container window to icon view
        set toolbar visible of container window to false
        set statusbar visible of container window to false
        set the bounds of container window to {100, 100, 660, 460}
        set viewOptions to icon view options of container window
        set arrangement of viewOptions to not arranged
        set icon size of viewOptions to 128
        set background picture of viewOptions to file ".background:bg.png"
        set position of item "${APP_NAME}.app" to {140, 165}
        set position of item "Applications" to {420, 165}
        close
        open
        delay 2
        update without registering applications
    end tell
end tell
APPLESCRIPT

hdiutil detach "$DEVICE" >/dev/null

echo "Compressing DMG..."
hdiutil convert "$DMG_TMP" -format UDZO -imagekey zlib-level=9 -o "$DMG_FINAL" >/dev/null
rm -rf "$DMG_STAGING" "$DMG_TMP"

if [[ "$SKIP_SIGN" == "false" ]]; then
    echo "Signing DMG..."
    codesign --force --timestamp --sign "$CERT_ID" "$DMG_FINAL"
fi

# ── Step 6: Notarize ──────────────────────────────────────────────────────────

if [[ "$SKIP_NOTARIZE" == "true" ]]; then
    echo "Skipping notarization (--skip-notarize)."
else
    echo "Submitting for notarization..."
    NOTARY_OUT=$(xcrun notarytool submit "$DMG_FINAL" \
        --keychain-profile "$KEYCHAIN_PROFILE" \
        --wait 2>&1) && NOTARY_OK=true || NOTARY_OK=false

    echo "$NOTARY_OUT"

    if [[ "$NOTARY_OK" == "false" ]]; then
        # Extract submission ID and download the detailed Apple diagnostic log.
        NOTARY_ID=$(echo "$NOTARY_OUT" | grep -Eo 'id: [0-9a-f-]+' | head -1 | awk '{print $2}')
        if [[ -n "$NOTARY_ID" ]]; then
            LOG_FILE="$BUILD_DIR/notarization-log-$(date +%Y%m%d-%H%M%S).json"
            xcrun notarytool log "$NOTARY_ID" \
                --keychain-profile "$KEYCHAIN_PROFILE" \
                > "$LOG_FILE" 2>/dev/null || true
            echo "Diagnostic log saved to: $LOG_FILE"
        fi
        exit 1
    fi

    echo "Stapling notarization ticket..."
    xcrun stapler staple "$DMG_FINAL"
fi

echo ""
echo "Done: $DMG_FINAL"
