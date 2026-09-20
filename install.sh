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
#   SILO_ALLOW_SHADOW  Install even when a .deb/.rpm/AUR silo is already here
#   SILO_INSTALL_MARKER  Where that install records itself
#                      (default: /usr/share/silo/install-method)
#
#   curl -sSfL https://raw.githubusercontent.com/dkam/silo/main/install.sh \
#     | VERSION=v0.5.1 INSTALL_DIR=$HOME/.local/bin sh
#
# This installs the binary only. On a server you almost certainly want the
# .deb or .rpm instead -- they bring the systemd unit, the silo user and
# /etc/silo. See packaging/README.md.

set -eu

usage() {
    cat <<'USAGE'
install.sh — install the silo binary

Usage:
  curl -sSfL .../install.sh | sh
  curl -sSfL .../install.sh | sh -s -- [--force] [--print]

  --force   Install the binary without asking, even where a native package
            would be the better choice
  --print   Say what would be installed and stop; write nothing
  --help    This text

Environment:
  VERSION            release tag to install, e.g. v0.5.1 (default: latest)
  INSTALL_DIR        where to put the binary (default: /usr/local/bin, or
                     ~/.local/bin when that is not writable)
  SILO_ALLOW_SHADOW  install even where a .deb/.rpm/AUR silo already is
USAGE
}

FORCE="${SILO_FORCE:-}"
PRINT=""
while [ $# -gt 0 ]; do
    case "$1" in
        --force|-f) FORCE=1 ;;
        --print|-n) PRINT=1 ;;
        --help|-h)  usage; exit 0 ;;
        # A typo must not fall through into installing something. This script
        # gets run as root, through a pipe, by people who cannot see it.
        *) printf 'install.sh: unknown option %s\n\n' "$1" >&2; usage >&2; exit 2 ;;
    esac
    shift
done

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

# A .deb, .rpm or AUR install leaves a marker naming the package manager that
# owns /usr/bin/silo. Installing over the top of one puts a second silo in
# /usr/local/bin, which precedes /usr/bin on most PATHs -- so an interactive
# shell gets the new binary while the systemd unit, which names /usr/bin/silo
# absolutely, goes on running the old one. Nothing reports a problem; the
# symptom turns up later as "I upgraded and the bug is still there".
: "${SILO_INSTALL_MARKER:=/usr/share/silo/install-method}"
if [ -z "${SILO_ALLOW_SHADOW:-}" ] && [ -f "$SILO_INSTALL_MARKER" ]; then
    owner=$(cat "$SILO_INSTALL_MARKER" 2>/dev/null) || owner=""
    [ -n "$owner" ] || owner="a package"
    die "silo on this machine was installed from ${owner}, and its binary belongs to
that package manager. Run \`silo upgrade\` for the command that upgrades it.

To install alongside it anyway, knowing the two will shadow each other:
  SILO_ALLOW_SHADOW=1 ... sh"
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
release_json=""
if [ -z "$VERSION" ]; then
    # No jq dependency: pull the release apart by hand. The whole document is
    # kept, not just the tag, because the names of the files it published are
    # the only correct source for the .deb and .rpm names -- nfpm decides those
    # and this script would otherwise be guessing at a convention.
    release_json=$(fetch_out "$SILO_LATEST_URL") || true
    VERSION=$(printf '%s' "$release_json" \
        | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' \
        | head -n 1) || true
    [ -n "$VERSION" ] || die "could not determine the latest release from $SILO_LATEST_URL; set VERSION=vX.Y.Z"
fi

# asset_url <filename-suffix> prints the download URL of the published file
# whose name ends that way, or nothing. Matching the whole tail rather than
# searching for ".deb" is what skips the .sha256 published beside each package.
asset_url() {
    printf '%s' "$release_json" | tr '{' '\n' | sed -n \
      's/.*"name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*"browser_download_url"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1 \2/p' \
      | while read -r n u; do
            case "$n" in
                *"$1") printf '%s %s\n' "$n" "$u"; break ;;
            esac
        done
}

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

# --- a native package, where there is one ----------------------------------
# This script installs a binary and nothing else. On a machine with a package
# manager that is the lesser install: the .deb and .rpm also bring the systemd
# unit, the silo service account and /etc/silo, and -- because the package owns
# the file -- `silo upgrade` can tell you how to upgrade it later.
#
# Homebrew is deliberately absent. There is no tap yet, so recommending one
# would send people to a formula that does not exist.
native=""
if [ "$goos" = linux ]; then
    if command -v pacman >/dev/null 2>&1; then
        native=aur
    elif command -v dpkg >/dev/null 2>&1; then
        native=deb
    elif command -v rpm >/dev/null 2>&1; then
        native=rpm
    fi
fi

# recommendation prints the case for the native package, or nothing.
recommendation() {
    [ -n "$native" ] || return 0
    case "$native" in
        deb) found=$(asset_url "_${goarch}.deb") ;;
        rpm) case "$goarch" in
                 amd64) found=$(asset_url ".x86_64.rpm") ;;
                 arm64) found=$(asset_url ".aarch64.rpm") ;;
                 *)     found="" ;;
             esac ;;
        *)   found="" ;;
    esac

    say ""
    case "$native" in
        aur)
            say "Recommended: this is an Arch system, and silo is in the AUR as silo-bin."
            say "It installs a systemd *user* unit, so silo runs as you:"
            say ""
            say "  yay -S silo-bin"
            say "  systemctl --user enable --now silo"
            ;;
        deb|rpm)
            say "Recommended: this machine has a package manager, and the .${native} installs"
            say "more than this script does -- the systemd unit, a silo service account and"
            say "/etc/silo -- and leaves the upgrade path to ${native}."
            if [ -n "$found" ]; then
                name=${found%% *}
                url=${found#* }
                say ""
                say "  curl -sSfLO $url"
                case "$native" in
                    deb) say "  sudo dpkg -i $name" ;;
                    rpm) say "  sudo rpm -i $name" ;;
                esac
            else
                say ""
                say "  https://github.com/${REPO}/releases/tag/${VERSION}"
            fi
            ;;
    esac
    say ""
    say "This script installs the binary alone, to ${INSTALL_DIR}."
}

if [ -n "$PRINT" ]; then
    say "Would install silo ${VERSION} (${goos}/${goarch}) to ${INSTALL_DIR}/silo"
    say "from ${SILO_RELEASE_BASE}/${VERSION}/${asset}"
    recommendation
    exit 0
fi

if [ -n "$native" ] && [ -z "$FORCE" ]; then
    recommendation
    say ""
    # `curl ... | sh` has no usable stdin -- stdin is the script -- so the
    # question goes to the terminal directly. When there is no terminal to ask
    # at, asking is not an option and hanging would be the worst of the three,
    # so it says its piece and carries on.
    if [ -r /dev/tty ]; then
        printf 'Install the binary anyway? [y/N] '
        read -r reply < /dev/tty || reply=""
        case "$reply" in
            y|Y|yes|YES|Yes) ;;
            *) say "Stopped. --force installs the binary regardless."; exit 0 ;;
        esac
    else
        say "(No terminal to ask at, so continuing. --force skips this notice,"
        say " --print shows it without installing.)"
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
