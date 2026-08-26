#!/bin/bash
# deploy.sh — builds ShortNerdCat.dmg and uploads it to the arbiter.
#
# Usage:
#   ./deploy.sh [--skip-build] [--skip-notarize]
#
# Environment variables (override defaults):
#   ARBITER_URL   base URL of the arbiter          (default: https://62.238.9.103)
#   UPLOAD_KEY    Bearer token for /admin/downloads/upload
#
# Sensitive credentials for signing/notarization are loaded from .env in the
# same directory as this script if it exists (CERT_ID, KEYCHAIN_PROFILE, …).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Load .env if present (signing credentials — never committed to git)
if [[ -f "$SCRIPT_DIR/.env" ]]; then
    # shellcheck disable=SC1090
    set -o allexport; source "$SCRIPT_DIR/.env"; set +o allexport
fi

# ── Defaults ──────────────────────────────────────────────────────────────────

ARBITER_URL="${ARBITER_URL:-https://62.238.9.103}"
UPLOAD_KEY="${UPLOAD_KEY:-6bf6076701ab343ac8342d00f5fd7b1df5874fae307918d0b247f6a89d65fe4b}"

SKIP_BUILD=false
SKIP_NOTARIZE=false

while [[ $# -gt 0 ]]; do
    case "$1" in
        --skip-build)    SKIP_BUILD=true;    shift ;;
        --skip-notarize) SKIP_NOTARIZE=true; shift ;;
        *) echo "Unknown option: $1"; exit 1 ;;
    esac
done

# Version stamp: YYYYMMDDHHmm — same format as Windows deploy.bat.
VERSION="$(date +%Y%m%d%H%M)"

# Must match build.sh's own BUILD_DIR exactly (REPO_ROOT/build/mac) -- this
# used to point at SCRIPT_DIR/build (snc/mac/build/) instead, a DIFFERENT
# directory build.sh never wrote to. That mismatch let a stale DMG sitting
# at the wrong path (accidentally committed to git, since only the
# repo-root /build/ is gitignored, not snc/mac/build/) get silently
# re-uploaded and re-stamped with a fresh version on every deploy for
# months, while the actually-rebuilt binary was ignored. See the
# 2026-08-10 incident: shipped dmg's Info.plist read 202605241501 while
# every download surface reported the current day's version.
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
BUILD_DIR="$REPO_ROOT/build/mac"
DMG="$BUILD_DIR/ShortNerdCat.dmg"

echo ""
echo "=== ShortNerdCat macOS Deploy ==="
echo "Version: $VERSION"
echo ""

# ── 1. Build ──────────────────────────────────────────────────────────────────

if [[ "$SKIP_BUILD" == "false" ]]; then
    echo "[1/2] Building..."
    BUILD_ARGS=(--version "$VERSION")
    [[ "$SKIP_NOTARIZE" == "true" ]] && BUILD_ARGS+=(--skip-notarize)
    bash "$SCRIPT_DIR/build.sh" "${BUILD_ARGS[@]}"
else
    echo "[1/2] Skipping build."
fi

if [[ ! -f "$DMG" ]]; then
    echo "ERROR: $DMG not found."
    exit 1
fi

# ── 2. Upload to arbiter ──────────────────────────────────────────────────────

echo "[2/2] Uploading version $VERSION to $ARBITER_URL ..."

HTTP_STATUS=$(curl -sk -L -H "Expect:" \
    -w "%{http_code}" -o /dev/null \
    -X POST "$ARBITER_URL/admin/downloads/upload" \
    -H "Authorization: Bearer $UPLOAD_KEY" \
    -F "binary_type=macos" \
    -F "version=$VERSION" \
    -F "file=@$DMG;filename=shortnerdcat.dmg")

echo "HTTP $HTTP_STATUS"

if [[ "$HTTP_STATUS" == "200" || "$HTTP_STATUS" == "303" ]]; then
    echo ""
    echo "Deploy complete: $ARBITER_URL/download"
else
    echo "ERROR: upload failed (HTTP $HTTP_STATUS)."
    exit 1
fi
