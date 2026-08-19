#!/bin/bash
# Build the SNC Go core as a static library for iOS.
# Outputs: ios-client/build/libsnc_core_{arm64,sim}.a + libsnc_core.h
#
# Requirements:
#   - macOS with Xcode 15+
#   - Go 1.21+ (GOOS=ios support)
#   - Run from the repo root: ./snc/ios/build.sh [version]
#
# The generated libsnc_core.h is auto-produced by CGO from //export declarations.
# It must be copied into SNCTunnel/GoCore/ in the Xcode project.

set -euo pipefail
cd "$(dirname "$0")/../.."

VERSION="${1:-$(date +%Y%m%d%H%M)}"
OUT="build/ios"
PKG="./snc/ios/cmd/snc-core"

SDK_DEVICE="$(xcrun --sdk iphoneos --show-sdk-path)"
SDK_SIM="$(xcrun --sdk iphonesimulator --show-sdk-path)"
CC_DEVICE="$(xcrun --sdk iphoneos --find clang)"
CC_SIM="$(xcrun --sdk iphonesimulator --find clang)"

LDFLAGS="-s -w -X tunnel_cat/snc/core.Version=${VERSION}"
TAGS="with_utls,ios"

mkdir -p "$OUT"

echo "==> Building snc-core for iOS device (arm64)..."
GOOS=ios GOARCH=arm64 CGO_ENABLED=1 \
  CC="$CC_DEVICE" \
  CGO_CFLAGS="-arch arm64 -isysroot $SDK_DEVICE -miphoneos-version-min=16.0" \
  CGO_LDFLAGS="-arch arm64 -isysroot $SDK_DEVICE" \
  go build -buildmode=c-archive \
    -tags "$TAGS" \
    -ldflags "$LDFLAGS" \
    -o "$OUT/libsnc_core_arm64.a" \
    "$PKG"

echo "==> Building snc-core for iOS simulator (arm64)..."
GOOS=ios GOARCH=arm64 CGO_ENABLED=1 \
  CC="$CC_SIM" \
  CGO_CFLAGS="-arch arm64 -isysroot $SDK_SIM -miphonesimulator-version-min=16.0 -target arm64-apple-ios16.0-simulator" \
  CGO_LDFLAGS="-arch arm64 -isysroot $SDK_SIM -target arm64-apple-ios16.0-simulator" \
  go build -buildmode=c-archive \
    -tags "$TAGS" \
    -ldflags "$LDFLAGS" \
    -o "$OUT/libsnc_core_sim_arm64.a" \
    "$PKG"

echo "==> Building snc-core for iOS simulator (x86_64)..."
GOOS=ios GOARCH=amd64 CGO_ENABLED=1 \
  CC="$CC_SIM" \
  CGO_CFLAGS="-arch x86_64 -isysroot $SDK_SIM -miphonesimulator-version-min=16.0 -target x86_64-apple-ios16.0-simulator" \
  CGO_LDFLAGS="-arch x86_64 -isysroot $SDK_SIM -target x86_64-apple-ios16.0-simulator" \
  go build -buildmode=c-archive \
    -tags "$TAGS" \
    -ldflags "$LDFLAGS" \
    -o "$OUT/libsnc_core_sim_x86_64.a" \
    "$PKG"

echo "==> Creating fat simulator library..."
lipo -create \
  "$OUT/libsnc_core_sim_arm64.a" \
  "$OUT/libsnc_core_sim_x86_64.a" \
  -output "$OUT/libsnc_core_sim.a"

echo "==> Copying header..."
# CGO generates the header alongside the first build output.
cp "$OUT/libsnc_core_arm64.h" "$OUT/libsnc_core.h" 2>/dev/null || true

echo ""
echo "Done. Outputs in $OUT/:"
ls -lh "$OUT/"libsnc_core*.{a,h} 2>/dev/null || true
echo ""
echo "Next: copy libsnc_core_arm64.a (or libsnc_core_sim.a for simulator)"
echo "      and libsnc_core.h into ios-client/SNCTunnel/GoCore/"
