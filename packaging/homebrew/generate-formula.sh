#!/usr/bin/env bash
#
# Generate Formula/silo.rb for the SiloDrive/homebrew-silo tap.
#
# Two modes, because there are two callers:
#
#   --dist DIR      read the checksums out of a local release directory. This
#                   is what CI does, straight after `build` -- the sidecars are
#                   already on disk, so there is no network round trip and no
#                   race against the release becoming visible.
#
#   (default)       fetch the .sha256 sidecars from the published release over
#                   HTTP. This is the by-hand path, for regenerating a formula
#                   for a release that already exists.
#
# Usage:
#   generate-formula.sh v0.5.1                     # fetch from the release
#   generate-formula.sh v0.5.1 --dist ./dist       # read local sidecars
#   generate-formula.sh v0.5.1 -o Formula/silo.rb  # write instead of stdout
#
# Writes to stdout unless -o is given, so it composes and can be diffed against
# the formula already in the tap without touching it.

set -euo pipefail

REPO="${SILO_REPO:-SiloDrive/silo}"
# Matches install.sh, and for the same reason: a tap pointed at a mirror or a
# gitea instance only needs this one variable moved.
RELEASE_BASE="${SILO_RELEASE_BASE:-https://github.com/${REPO}/releases/download}"

TAG=""
DIST=""
OUT=""

die() { printf 'generate-formula.sh: %s\n' "$*" >&2; exit 1; }

while [ $# -gt 0 ]; do
    case "$1" in
        --dist) DIST="${2:-}"; shift 2 ;;
        -o)     OUT="${2:-}";  shift 2 ;;
        -h|--help) sed -n '2,/^set -euo/p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
        -*)     die "unknown option: $1" ;;
        *)      TAG="$1"; shift ;;
    esac
done

[ -n "$TAG" ] || die "no version given (e.g. v0.5.1)"

# Accept 0.5.1 or v0.5.1 and settle on both spellings once. The tag carries the
# v -- release assets are named by it -- and the formula's `version` field must
# not, or `brew` compares versions against a string it cannot parse.
case "$TAG" in
    v*) VERSION="${TAG#v}" ;;
    *)  VERSION="$TAG"; TAG="v${TAG}" ;;
esac

BASE_URL="${RELEASE_BASE}/${TAG}"

# The four platforms build.yml publishes. Homebrew needs every one of them:
# a formula missing a bottle for the running platform fails at install time,
# not at audit time.
TARGETS="darwin-arm64 darwin-amd64 linux-arm64 linux-amd64"

sha_for() {
    target="$1"
    asset="silo-${TAG}-${target}.tar.gz"
    if [ -n "$DIST" ]; then
        f="${DIST}/${asset}.sha256"
        [ -f "$f" ] || die "missing ${f} -- was the build artifact downloaded?"
        # The sidecar is `<hash>  <filename>`, as sha256sum writes it.
        awk '{print $1}' "$f"
    else
        curl -sfL "${BASE_URL}/${asset}.sha256" | awk '{print $1}'
    fi
}

[ -n "$DIST" ] && echo "Reading checksums from ${DIST}" >&2 \
               || echo "Fetching checksums for ${TAG}" >&2

for target in $TARGETS; do
    sha=$(sha_for "$target")
    [ -n "$sha" ] || die "no sha256 for silo-${TAG}-${target}.tar.gz"
    # 64 hex characters, or something upstream of here is wrong and the formula
    # would install a binary nobody checked.
    case "$sha" in
        [0-9a-f]*) [ ${#sha} -eq 64 ] || die "sha256 for ${target} is ${#sha} chars, not 64: ${sha}" ;;
        *) die "sha256 for ${target} is not hex: ${sha}" ;;
    esac
    eval "SHA_$(echo "$target" | tr '-' '_')=\$sha"
    echo "  ${target}: ${sha}" >&2
done

formula=$(cat <<FORMULA
class Silo < Formula
  desc "Single-binary file sync server with per-library end-to-end encryption"
  homepage "https://github.com/${REPO}"
  version "${VERSION}"
  license "AGPL-3.0-only"

  on_macos do
    on_arm do
      url "${BASE_URL}/silo-${TAG}-darwin-arm64.tar.gz"
      sha256 "${SHA_darwin_arm64}"
    end
    on_intel do
      url "${BASE_URL}/silo-${TAG}-darwin-amd64.tar.gz"
      sha256 "${SHA_darwin_amd64}"
    end
  end

  on_linux do
    on_arm do
      url "${BASE_URL}/silo-${TAG}-linux-arm64.tar.gz"
      sha256 "${SHA_linux_arm64}"
    end
    on_intel do
      url "${BASE_URL}/silo-${TAG}-linux-amd64.tar.gz"
      sha256 "${SHA_linux_amd64}"
    end
  end

  def install
    bin.install "silo"

    # Which package manager owns this binary. The binary inside the release
    # tarball is the tarball build, stamped InstallMethod=tarball, so without
    # this marker \`silo upgrade\` would tell a Homebrew user to pipe
    # install.sh into sh -- which writes a second silo to /usr/local/bin,
    # ahead of the Cellar one on PATH. The .deb, the .rpm and the AUR package
    # each write the same file; this is the fourth.
    #
    # internal/upgrade.MarkerPath reads <prefix>/share/silo/install-method,
    # derived from the binary's own location. That lands here whether
    # os.Executable resolves the symlink (Linux, via /proc/self/exe, giving
    # the Cellar path) or not (macOS, giving #{HOMEBREW_PREFIX}/bin/silo,
    # whose share/silo is the symlink \`brew link\` made to this one).
    (share/"silo").mkpath
    (share/"silo/install-method").write "homebrew\n"
  end

  test do
    # \`silo version\` prints the version with no leading v -- normalizeVersion
    # in cmd/silo/main.go strips it -- so asserting "v#{version}" here silently
    # fails for every release. Compare against the bare string.
    assert_equal version.to_s, shell_output("#{bin}/silo version").strip
  end
end
FORMULA
)

if [ -n "$OUT" ]; then
    mkdir -p "$(dirname "$OUT")"
    printf '%s\n' "$formula" > "$OUT"
    echo "Wrote ${OUT} (${TAG})" >&2
else
    printf '%s\n' "$formula"
fi
