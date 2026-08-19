#!/usr/bin/env bash
# deploy.sh — build the Linux .deb package and upload to the arbiter.
#
# Usage:
#   ./deploy/linux/deploy.sh [--skip-build]
#
# Optional env var overrides:
#   VERSION      override build version (default: YYYYMMDDHHmm)
#   ARBITER_HOST SSH target for key fetch (required, e.g. root@your-arbiter-host)
#   ARBITER_URL  HTTPS base URL           (derived from ARBITER_HOST if not set)
#   UPLOAD_KEY   bearer token             (fetched from arbiter via SSH if not set)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

ARBITER_URL="${ARBITER_URL:?set ARBITER_URL to your own arbiter, e.g. https://your-arbiter-host}"
UPLOAD_KEY="${UPLOAD_KEY:?set UPLOAD_KEY to your own arbiter's admin upload key}"
SKIP_BUILD=false

while [[ $# -gt 0 ]]; do
    case "$1" in
        --skip-build) SKIP_BUILD=true; shift ;;
        *) echo "Unknown option: $1"; exit 1 ;;
    esac
done

VERSION="${VERSION:-$(date +%Y%m%d%H%M)}"
OUT_DIR="${REPO_ROOT}/build/linux"

echo ""
echo "=== ShortNerdCat Linux Deploy ==="
echo "Version: ${VERSION}"
echo ""

# ── 1. Build ───────────────────────────────────────────────────────────────────

if ! $SKIP_BUILD; then
    echo "[1/3] Building .deb (version ${VERSION})..."
    VERSION="${VERSION}" bash "${SCRIPT_DIR}/build.sh" --skip-deps
else
    echo "[1/3] Skipping build."
fi

DEB_FILE="${OUT_DIR}/shortnerdcat_${VERSION}_amd64.deb"
[[ -f "$DEB_FILE" ]] || { echo "ERROR: .deb not found at ${DEB_FILE}"; exit 1; }

# ── 2. Upload to arbiter ───────────────────────────────────────────────────────

echo "[2/3] Uploading version ${VERSION} to ${ARBITER_URL}..."
HTTP_STATUS=$(curl -sk -o /tmp/snc-linux-upload.json -w "%{http_code}" \
    -X POST "${ARBITER_URL}/admin/downloads/upload" \
    -H "Authorization: Bearer ${UPLOAD_KEY}" \
    -H "Accept: application/json" \
    -F "binary_type=linux" \
    -F "version=${VERSION}" \
    -F "file=@${DEB_FILE}")

if [[ "$HTTP_STATUS" != "200" && "$HTTP_STATUS" != "303" ]]; then
    echo "ERROR: Upload failed (HTTP ${HTTP_STATUS}):"
    cat /tmp/snc-linux-upload.json 2>/dev/null || true
    exit 1
fi

echo ""
echo "Deploy complete (version ${VERSION}): ${ARBITER_URL}/download"
