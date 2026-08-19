#!/usr/bin/env bash
# Build the ShortNerdCat Linux desktop client.
# Self-installs Go and required system packages if missing (requires sudo for packages).
#
# Usage: ./deploy/linux/build.sh [--out <dir>] [--arch <amd64|arm64>] [--skip-deps]
#
# Produces:
#   <out>/shortnerdcat          — standalone ELF binary
#   <out>/shortnerdcat.deb      — Debian/Ubuntu package

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

OUT_DIR="${REPO_ROOT}/build/linux"
ARCH="amd64"
SKIP_DEPS=0

while [[ $# -gt 0 ]]; do
    case "$1" in
        --out)        OUT_DIR="$2"; shift 2 ;;
        --arch)       ARCH="$2";    shift 2 ;;
        --skip-deps)  SKIP_DEPS=1;  shift   ;;
        *)            echo "Unknown option: $1"; exit 1 ;;
    esac
done

# ── Go version requirement ─────────────────────────────────────────────────────

GO_REQUIRED="1.26.2"
GO_INSTALL_DIR="/usr/local/go"
GO_DL_BASE="https://dl.google.com/go"

# Returns 0 if $1 >= $2 (semver compare via sort -V)
version_ge() {
    [ "$(printf '%s\n' "$1" "$2" | sort -V | head -1)" = "$2" ]
}

find_go() {
    for candidate in "$(command -v go 2>/dev/null || true)" "${GO_INSTALL_DIR}/bin/go" "${HOME}/go/bin/go"; do
        [[ -z "$candidate" || ! -x "$candidate" ]] && continue
        v=$("$candidate" version 2>/dev/null | awk '{print $3}' | sed 's/go//')
        if version_ge "$v" "$GO_REQUIRED"; then
            echo "$candidate"
            return 0
        fi
    done
    return 1
}

install_go() {
    local goarch="$ARCH"
    local tarball="go${GO_REQUIRED}.linux-${goarch}.tar.gz"
    local url="${GO_DL_BASE}/${tarball}"
    local tmpdir
    tmpdir="$(mktemp -d)"

    echo "  Downloading Go ${GO_REQUIRED} from ${url} ..."
    if command -v curl &>/dev/null; then
        curl -fsSL -o "${tmpdir}/${tarball}" "$url"
    else
        wget -q -O "${tmpdir}/${tarball}" "$url"
    fi

    echo "  Installing Go to ${GO_INSTALL_DIR} ..."
    sudo rm -rf "${GO_INSTALL_DIR}"
    sudo tar -C /usr/local -xzf "${tmpdir}/${tarball}"
    rm -rf "${tmpdir}"
    echo "  Go ${GO_REQUIRED} installed at ${GO_INSTALL_DIR}/bin/go"
}

# ── System package dependencies ────────────────────────────────────────────────

APT_PACKAGES=(
    build-essential
    pkg-config
    libgtk-3-dev
    libayatana-appindicator3-dev
    dpkg-dev
)

# webkit2gtk-4.0 was removed in Ubuntu 24.04; fall back to 4.1.
# Flags are passed via CGO_CFLAGS/CGO_LDFLAGS so no build tags needed.
WEBKIT_PKGCONFIG=""
detect_webkit_pkg() {
    if apt-cache show libwebkit2gtk-4.0-dev &>/dev/null 2>&1; then
        WEBKIT_PKGCONFIG="webkit2gtk-4.0"
        APT_PACKAGES+=(libwebkit2gtk-4.0-dev)
    elif apt-cache show libwebkit2gtk-4.1-dev &>/dev/null 2>&1; then
        WEBKIT_PKGCONFIG="webkit2gtk-4.1"
        APT_PACKAGES+=(libwebkit2gtk-4.1-dev)
    else
        echo "error: neither libwebkit2gtk-4.0-dev nor libwebkit2gtk-4.1-dev found in apt cache"
        exit 1
    fi
    echo "WebKit: using ${WEBKIT_PKGCONFIG}"
}

ensure_apt_deps() {
    if ! command -v dpkg &>/dev/null; then
        echo "warn: dpkg not found — skipping apt dependency check (non-Debian system?)"
        return
    fi

    detect_webkit_pkg

    local missing=()
    for pkg in "${APT_PACKAGES[@]}"; do
        if ! dpkg -s "$pkg" &>/dev/null 2>&1; then
            missing+=("$pkg")
        fi
    done

    if [[ ${#missing[@]} -eq 0 ]]; then
        echo "System packages: all present."
        return
    fi

    echo "Missing packages: ${missing[*]}"
    echo "Installing via apt-get ..."
    sudo apt-get update -qq
    sudo apt-get install -y "${missing[@]}"
    echo "System packages installed."
}

# ── Dependency setup ───────────────────────────────────────────────────────────

if [[ "$SKIP_DEPS" -eq 0 ]]; then
    echo "=== Checking system packages ==="
    ensure_apt_deps

    echo "=== Checking Go ==="
    if GO_BIN="$(find_go)"; then
        echo "Go OK: $("$GO_BIN" version)"
    else
        echo "Go ${GO_REQUIRED}+ not found — installing."
        install_go
        GO_BIN="${GO_INSTALL_DIR}/bin/go"
        echo "Go OK: $("$GO_BIN" version)"
    fi
else
    echo "=== Skipping dependency check (--skip-deps) ==="
    GO_BIN="$(find_go)" || GO_BIN="go"
    # Detect webkit via pkg-config (packages already installed).
    if pkg-config --exists webkit2gtk-4.1 2>/dev/null; then
        WEBKIT_PKGCONFIG="webkit2gtk-4.1"
    else
        WEBKIT_PKGCONFIG="webkit2gtk-4.0"
    fi
    echo "WebKit: using ${WEBKIT_PKGCONFIG}"
fi

export PATH="${GO_INSTALL_DIR}/bin:${PATH}"

# Resolve webkit CGO flags now that packages are installed.
WEBKIT_CFLAGS="$(pkg-config --cflags "${WEBKIT_PKGCONFIG}")"
WEBKIT_LDFLAGS="$(pkg-config --libs   "${WEBKIT_PKGCONFIG}")"
echo "WebKit cflags: ${WEBKIT_CFLAGS:0:60}..."

# ── Build ──────────────────────────────────────────────────────────────────────

DEB_VERSION="${VERSION:-$(date +%Y%m%d%H%M)}"

mkdir -p "${OUT_DIR}"

BINARY="${OUT_DIR}/shortnerdcat"
PKG_DIR="${OUT_DIR}/pkg"

echo ""
echo "=== Building linux/${ARCH} ==="

cd "${REPO_ROOT}"
CGO_CFLAGS="${WEBKIT_CFLAGS}" CGO_LDFLAGS="${WEBKIT_LDFLAGS}" \
GOOS=linux GOARCH="${ARCH}" CGO_ENABLED=1 \
    "${GO_BIN}" build \
        -tags "linux" \
        -ldflags "-s -w -X tunnel_cat/snc/core.Version=${DEB_VERSION}" \
        -o "${BINARY}" \
        ./snc/linux/cmd/shortnerdcat

echo "Binary: ${BINARY}"

# ── Debian package ─────────────────────────────────────────────────────────────

echo ""
echo "=== Building .deb ==="

DEB_NAME="shortnerdcat"
DEB_ARCH="${ARCH}"

DEB_STAGING="${PKG_DIR}/${DEB_NAME}_${DEB_VERSION}_${DEB_ARCH}"
rm -rf "${DEB_STAGING}"
mkdir -p "${DEB_STAGING}/DEBIAN"
mkdir -p "${DEB_STAGING}/usr/bin"
mkdir -p "${DEB_STAGING}/usr/share/applications"
mkdir -p "${DEB_STAGING}/usr/share/polkit-1/actions"
mkdir -p "${DEB_STAGING}/lib/systemd/system"
mkdir -p "${DEB_STAGING}/var/lib/shortnerdcat"
mkdir -p "${DEB_STAGING}/var/log/shortnerdcat"

install -m 0755 "${BINARY}" "${DEB_STAGING}/usr/bin/shortnerdcat"

install -m 0644 \
    "${SCRIPT_DIR}/shortnerdcat.desktop" \
    "${DEB_STAGING}/usr/share/applications/shortnerdcat.desktop"

install -m 0644 \
    "${SCRIPT_DIR}/org.shortnerdcat.policy" \
    "${DEB_STAGING}/usr/share/polkit-1/actions/org.shortnerdcat.policy"

install -m 0644 \
    "${SCRIPT_DIR}/shortnerdcat.service" \
    "${DEB_STAGING}/lib/systemd/system/shortnerdcat.service"

# Determine runtime library names that match the dev packages installed above.
if [[ "$WEBKIT_PKGCONFIG" == "webkit2gtk-4.1" ]]; then
    WEBKIT_RUNTIME="libwebkit2gtk-4.1-0"
    APPIND_RUNTIME="libayatana-appindicator3-1"
else
    WEBKIT_RUNTIME="libwebkit2gtk-4.0-37"
    APPIND_RUNTIME="libappindicator3-1"
fi

sed \
    -e "s/DEB_VERSION_PLACEHOLDER/${DEB_VERSION}/" \
    -e "s/DEB_ARCH_PLACEHOLDER/${DEB_ARCH}/" \
    -e "s/DEB_WEBKIT_PLACEHOLDER/${WEBKIT_RUNTIME}/" \
    -e "s/DEB_APPIND_PLACEHOLDER/${APPIND_RUNTIME}/" \
    "${SCRIPT_DIR}/debian/control" > "${DEB_STAGING}/DEBIAN/control"
cp "${SCRIPT_DIR}/debian/postinst" "${DEB_STAGING}/DEBIAN/postinst"
cp "${SCRIPT_DIR}/debian/prerm"    "${DEB_STAGING}/DEBIAN/prerm"
chmod 0755 "${DEB_STAGING}/DEBIAN/postinst" "${DEB_STAGING}/DEBIAN/prerm"

DEB_FILE="${OUT_DIR}/${DEB_NAME}_${DEB_VERSION}_${DEB_ARCH}.deb"
dpkg-deb --build --root-owner-group "${DEB_STAGING}" "${DEB_FILE}"

sha256sum "${BINARY}"    > "${BINARY}.sha256"
sha256sum "${DEB_FILE}"  > "${DEB_FILE}.sha256"

echo ""
echo "=== Done ==="
echo "  Binary:  ${BINARY}"
echo "  Package: ${DEB_FILE}"
