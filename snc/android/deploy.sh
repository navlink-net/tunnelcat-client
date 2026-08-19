#!/usr/bin/env bash
# deploy.sh — build release APK and upload to the arbiter via HTTP admin endpoint.
#
# Usage:
#   VERSION=20260516 ./deploy.sh [--skip-build]
#
# Optional env var overrides:
#   ARBITER_HOST   SSH target (required, e.g. root@your-arbiter-host)
#   ARBITER_URL    HTTPS base URL (derived from ARBITER_HOST if not set)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
APK="${SCRIPT_DIR}/app/build/outputs/apk/release/app-release.apk"
ARBITER_HOST="${ARBITER_HOST:?set ARBITER_HOST to your own arbiter SSH target, e.g. root@your-arbiter-host}"
ARBITER_IP="${ARBITER_HOST##*@}"
ARBITER_URL="${ARBITER_URL:-https://${ARBITER_IP}}"
SKIP_BUILD=false

while [[ $# -gt 0 ]]; do
    case "$1" in
        --skip-build) SKIP_BUILD=true; shift ;;
        *) echo "Unknown option: $1"; exit 1 ;;
    esac
done

if [[ -z "${VERSION:-}" ]]; then
    echo "ERROR: VERSION env var is required (e.g. export VERSION=20260516)"
    exit 1
fi

if ! $SKIP_BUILD; then
    echo "=== Building release APK (version ${VERSION}) ==="
    pushd "$SCRIPT_DIR" > /dev/null
    ./gradlew clean assembleRelease -PappVersion="${VERSION}"
    popd > /dev/null
fi

[[ -f "$APK" ]] || { echo "ERROR: APK not found at ${APK}"; exit 1; }

# Verify VERSION actually matches what's baked into the APK's own manifest
# (versionName) rather than trusting the env var blindly -- confirmed real
# bug, 2026-08-10: a --skip-build deploy uploaded an already-built APK under
# a VERSION label that didn't match its own versionName (still an older
# build). Android compares the *installed* app's versionCode against the
# new APK's versionCode, not the server's advertised label -- since the
# actual APK's versionCode hadn't increased, Android silently no-ops the
# "install" (no error surfaced anywhere in our code), the device's reported
# version never changes, and UpdateChecker nags forever since the server
# keeps advertising a version number the file itself was never built with.
# This is exactly why --skip-build exists to be reused across quick re-runs,
# so this check is a guard against exactly that convenience footgun, not a
# reason to remove --skip-build.
AAPT="$(command -v aapt || true)"
if [[ -z "$AAPT" ]]; then
    for candidate in "$HOME"/AppData/Local/Android/Sdk/build-tools/*/aapt.exe /usr/local/android-sdk/build-tools/*/aapt; do
        [[ -x "$candidate" ]] && AAPT="$candidate" && break
    done
fi
if [[ -n "$AAPT" ]]; then
    APK_VERSION_NAME=$("$AAPT" dump badging "$APK" 2>/dev/null | grep -oP "(?<=versionName=')[^']+")
    if [[ -z "$APK_VERSION_NAME" ]]; then
        echo "WARNING: could not read versionName from $APK via aapt -- skipping version-match check"
    elif [[ "$APK_VERSION_NAME" != "$VERSION" ]]; then
        echo "ERROR: VERSION=${VERSION} does not match the APK's own versionName=${APK_VERSION_NAME}."
        echo "       This APK was built with a different -PappVersion (likely a stale --skip-build reuse)."
        echo "       Re-run without --skip-build, or set VERSION=${APK_VERSION_NAME} to match what's actually in the file."
        exit 1
    fi
else
    echo "WARNING: aapt not found -- cannot verify VERSION matches the APK's own versionName. Proceeding without the check."
fi

SSH_OPTS="-o StrictHostKeyChecking=accept-new -o ConnectTimeout=15"

# Read or generate the upload key on the arbiter.
echo "=== Fetching upload key from ${ARBITER_HOST} ==="
UPLOAD_KEY=$(ssh $SSH_OPTS "${ARBITER_HOST}" \
    "grep -oP '(?<=^UPLOAD_KEY=).+' /etc/snc-arbiter/secrets.env 2>/dev/null || grep -oP '(?<=^SNC_UPLOAD_KEY=).+' /etc/snc/arbiter.env 2>/dev/null || true")

if [[ -z "$UPLOAD_KEY" ]]; then
    echo "--- SNC_UPLOAD_KEY not set; generating and configuring on arbiter..."
    UPLOAD_KEY=$(openssl rand -hex 32)
    # Pass the key as $1 so no quoting issues inside the remote script.
    ssh $SSH_OPTS "${ARBITER_HOST}" bash -s -- "$UPLOAD_KEY" <<'REMOTE'
set -e
KEY="$1"
ENV_FILE=/etc/snc/arbiter.env
UNIT=/etc/systemd/system/snc-arbiter.service

echo "SNC_UPLOAD_KEY=${KEY}" >> "$ENV_FILE"

# Inject --upload-key flag into ExecStart after --log line, if not already present.
# sed appends a continuation \ to the --log line and inserts --upload-key on the next line.
if ! grep -q -- '--upload-key' "$UNIT"; then
    sed -i '/--log.*SNC_LOG/{
s/$/ \\/
a\    --upload-key  ${SNC_UPLOAD_KEY}
}' "$UNIT"
fi

systemctl daemon-reload
systemctl restart snc-arbiter
sleep 3
systemctl is-active snc-arbiter && echo "arbiter running"
REMOTE
    echo "--- Arbiter reconfigured with upload key."
fi

echo "=== Uploading APK to ${ARBITER_URL} (version ${VERSION}) ==="
HTTP_STATUS=$(curl -sk -o /tmp/snc-upload-response.json -w "%{http_code}" \
    -X POST "${ARBITER_URL}/admin/downloads/upload" \
    -H "Authorization: Bearer ${UPLOAD_KEY}" \
    -H "Accept: application/json" \
    -F "binary_type=android" \
    -F "version=${VERSION}" \
    -F "file=@${APK}")

if [[ "$HTTP_STATUS" != "200" ]]; then
    echo "ERROR: Upload failed (HTTP ${HTTP_STATUS}):"
    cat /tmp/snc-upload-response.json 2>/dev/null || true
    exit 1
fi

echo "Deploy complete (version ${VERSION}): $(cat /tmp/snc-upload-response.json)"
