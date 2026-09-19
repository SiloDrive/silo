#!/bin/sh
#
# Silo installer.
#
#   curl -sSfL https://raw.githubusercontent.com/dkam/silo/main/install.sh | sh
#   wget -qO- https://raw.githubusercontent.com/dkam/silo/main/install.sh | sh
#
# Environment:
#   VERSION            release tag to install, e.g. v0.5.1 (default: latest)
#   INSTALL_DIR        where to put the binary (default: /usr/local/bin, or
#                      ~/.local/bin when that is not writable)
#   SILO_RELEASE_BASE  release download root; the tag and then the asset name
#                      are appended (default: the GitHub releases URL)
#   SILO_LATEST_URL    URL returning JSON with "tag_name" (default: the GitHub
#                      API). Gitea serves the same shape at
#                      /api/v1/repos/<owner>/<repo>/releases/latest
#
#   curl -sSfL https://raw.githubusercontent.com/dkam/silo/main/install.sh \
#     | VERSION=v0.5.1 INSTALL_DIR=$HOME/.local/bin sh
#
# This installs the binary only. On a server you almost certainly want the
# .deb or .rpm instead -- they bring the systemd unit, the silo user and
# /etc/silo. See packaging/README.md.

set -eu

REPO="dkam/silo"
: "${VERSION:=}"
: "${INSTALL_DIR:=}"

# Where releases are served from. Overridable in one place so the script can be
# pointed at a Gitea instance, a mirror, or a local directory for testing,
# without touching anything below.
#   SILO_RELEASE_BASE  release tag is appended, then the asset filename
#   SILO_LATEST_URL    returns JSON containing "tag_name": "vX.Y.Z"
: "${SILO_RELEASE_BASE:=https://github.com/${REPO}/releases/download}"
: "${SILO_LATEST_URL:=https://api.github.com/repos/${REPO}/releases/latest}"

say()  { printf '%s\n' "$*"; }
die()  { printf 'install.sh: %s\n' "$*" >&2; exit 1; }

# --- fetch -----------------------------------------------------------------
# One of curl or wget, chosen once, so the rest of the script does not care.
if command -v curl >/dev/null 2>&1; then
    fetch()    { curl -sSfL "$1" -o "$2"; }
    fetch_out() { curl -sSfL "$1"; }
elif command -v wget >/dev/null 2>&1; then
    fetch()    { wget -qO "$2" "$1"; }
    fetch_out() { wget -qO- "$1"; }
else
    die "neither curl nor wget found"
fi

# --- platform --------------------------------------------------------------
os=$(uname -s)
case "$os" in
    Linux)  goos=linux  ;;
    Darwin) goos=darwin ;;
    *)      die "unsupported OS: $os (releases cover Linux and macOS)" ;;
esac

arch=$(uname -m)
case "$arch" in
    x86_64|amd64)  goarch=amd64 ;;
    aarch64|arm64) goarch=arm64 ;;
    *)             die "unsupported architecture: $arch (releases cover amd64 and arm64)" ;;
esac

# --- version ---------------------------------------------------------------
if [ -z "$VERSION" ]; then
    # No jq dependency: pull tag_name out of the release JSON by hand.
    VERSION=$(fetch_out "$SILO_LATEST_URL" \
        | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' \
        | head -n 1) || true
    [ -n "$VERSION" ] || die "could not determine the latest release from $SILO_LATEST_URL; set VERSION=vX.Y.Z"
fi

# Release assets are named by the tag exactly as it appears, leading v included.
asset="silo-${VERSION}-${goos}-${goarch}.tar.gz"
base="${SILO_RELEASE_BASE}/${VERSION}"

# --- destination -----------------------------------------------------------
# Resolved before downloading, so a permissions problem is reported before the
# network work rather than after it.
if [ -z "$INSTALL_DIR" ]; then
    if [ -w /usr/local/bin ] 2>/dev/null; then
        INSTALL_DIR=/usr/local/bin
    elif [ "$(id -u)" = "0" ]; then
        INSTALL_DIR=/usr/local/bin
    else
        INSTALL_DIR="$HOME/.local/bin"
        say "install.sh: /usr/local/bin is not writable; installing to $INSTALL_DIR"
    fi
fi
mkdir -p "$INSTALL_DIR" || die "cannot create $INSTALL_DIR"
[ -w "$INSTALL_DIR" ] || die "$INSTALL_DIR is not writable (re-run with sudo, or set INSTALL_DIR)"

# --- checksum tool ---------------------------------------------------------
if command -v sha256sum >/dev/null 2>&1; then
    checksum() { sha256sum -c "$1"; }
elif command -v shasum >/dev/null 2>&1; then
    checksum() { shasum -a 256 -c "$1"; }
else
    die "neither sha256sum nor shasum found; cannot verify the download"
fi

# --- download and verify ---------------------------------------------------
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

say "Downloading silo ${VERSION} (${goos}/${goarch})"
fetch "${base}/${asset}"         "${tmp}/${asset}"         || die "download failed: ${base}/${asset}"
fetch "${base}/${asset}.sha256"  "${tmp}/${asset}.sha256"  || die "download failed: ${base}/${asset}.sha256"

# The .sha256 published by the build names the file by basename, so the check
# has to run from the directory holding it.
( cd "$tmp" && checksum "${asset}.sha256" >/dev/null ) \
    || die "checksum mismatch for ${asset} -- not installing"

tar -xzf "${tmp}/${asset}" -C "$tmp" silo || die "could not extract silo from ${asset}"

# Install via a temporary name and rename into place: mv within one filesystem
# is atomic, so a running silo is never served a half-written binary.
chmod 0755 "${tmp}/silo"
mv "${tmp}/silo" "${INSTALL_DIR}/silo.new" || die "could not write to ${INSTALL_DIR}"
mv "${INSTALL_DIR}/silo.new" "${INSTALL_DIR}/silo"

say "Installed ${INSTALL_DIR}/silo"
"${INSTALL_DIR}/silo" version || true

case ":${PATH}:" in
    *":${INSTALL_DIR}:"*) ;;
    *) say ""
       say "Note: ${INSTALL_DIR} is not on your PATH. Add it:"
       say "  export PATH=\"${INSTALL_DIR}:\$PATH\""
       ;;
esac
