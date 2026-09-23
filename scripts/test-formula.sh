#!/bin/sh
#
# Tests for packaging/homebrew/generate-formula.sh.
#
# The formula is generated text, and the things that go wrong with it are
# things that are absent from that text -- a missing install-method marker, a
# checksum that was never fetched -- so this harness runs the real generator
# against a fake release directory and reads what it produced.
#
# It deliberately does not run `brew`. Homebrew is not installable on the CI
# runner and would not be the thing under test anyway: what is under test is
# whether this repository emits a correct formula, not whether Homebrew can
# interpret one.

set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
gen="$root/packaging/homebrew/generate-formula.sh"
failures=0

ok()  { printf '  ok    %s\n' "$*"; }
bad() { printf '  FAIL  %s\n' "$*"; failures=$((failures + 1)); }

# --- sandbox ---------------------------------------------------------------
# A dist directory holding the four .sha256 sidecars the build job publishes,
# and nothing else: the generator must not need the tarballs themselves.
sandbox() {
    T=$(mktemp -d)
    mkdir -p "$T/dist"
    i=0
    for target in darwin-arm64 darwin-amd64 linux-arm64 linux-amd64; do
        # Distinct hashes per platform, so a formula that pasted one checksum
        # into every block fails rather than passing by coincidence.
        hash=$(printf '%s' "$target" | sha256sum | awk '{print $1}')
        printf '%s  silo-v9.9.9-%s.tar.gz\n' "$hash" "$target" \
            > "$T/dist/silo-v9.9.9-${target}.tar.gz.sha256"
        eval "want_$(echo "$target" | tr '-' '_')=\$hash"
        i=$((i + 1))
    done
}

generate() { "$gen" "$@" --dist "$T/dist" 2>"$T/err"; }

# --- the marker --------------------------------------------------------------
# The reason this file exists. The binary inside the release tarball was
# stamped InstallMethod=tarball by the build job, because it is the same
# binary the tarball ships. If the formula does not write a marker over the
# top of that stamp, `silo upgrade` tells a brew user to pipe install.sh into
# sh, which writes a second silo to /usr/local/bin and shadows the Cellar one.
# deb, rpm and the AUR each write this marker; Homebrew is the fourth.
sandbox
out=$(generate v9.9.9) || out=""
case "$out" in
    *install-method*) ok "the formula writes an install-method marker" ;;
    *) bad "no install-method marker: a brew install would claim it came from a tarball" ;;
esac
# internal/upgrade.MarkerPath derives <prefix>/share/silo/install-method from
# the binary's own location, so the formula has to write that exact path and
# not, say, etc/ or libexec/.
case "$out" in
    *'share/silo/install-method'*) ok "the marker is at the path MarkerPath looks in" ;;
    *) bad "marker is not at <prefix>/share/silo/install-method" ;;
esac
case "$out" in
    *'"homebrew'*|*"'homebrew"*|*homebrew\\n*) ok "the marker says homebrew" ;;
    *) bad "the marker does not contain the word homebrew" ;;
esac
rm -rf "$T"

# --- checksums ---------------------------------------------------------------
sandbox
out=$(generate v9.9.9) || out=""
miss=0
for target in darwin-arm64 darwin-amd64 linux-arm64 linux-amd64; do
    eval "want=\$want_$(echo "$target" | tr '-' '_')"
    case "$out" in *"$want"*) ;; *) miss=$((miss + 1)) ;; esac
done
[ "$miss" -eq 0 ] && ok "carries all four platform checksums, distinctly" \
                  || bad "$miss of 4 checksums missing from the formula"

# Four url lines and four sha256 lines, or a platform is silently absent.
urls=$(printf '%s\n' "$out" | grep -c '^ *url ' || true)
shas=$(printf '%s\n' "$out" | grep -c '^ *sha256 ' || true)
[ "$urls" = 4 ] && ok "four url blocks" || bad "expected 4 url lines, got $urls"
[ "$shas" = 4 ] && ok "four sha256 blocks" || bad "expected 4 sha256 lines, got $shas"
rm -rf "$T"

# --- the organisation --------------------------------------------------------
# The repository moved to the SiloDrive org. A formula still pointing at
# dkam/silo works only for as long as GitHub keeps the redirect.
sandbox
out=$(generate v9.9.9) || out=""
case "$out" in
    *SiloDrive/silo*) ok "urls point at SiloDrive/silo" ;;
    *) bad "formula does not point at the SiloDrive org" ;;
esac
case "$out" in
    *dkam*) bad "formula still mentions dkam" ;;
    *) ok "no stale dkam references" ;;
esac
rm -rf "$T"

# --- version spelling --------------------------------------------------------
# The tag carries the v and the formula's version field must not, or brew
# compares against a string it cannot parse. Both spellings of the argument
# have to land in the same place.
sandbox
a=$(generate v9.9.9) || a=""
b=$(generate 9.9.9)  || b=""
[ "$a" = "$b" ] && ok "v9.9.9 and 9.9.9 generate the same formula" \
                || bad "leading v changes the output"
case "$a" in
    *'version "9.9.9"'*) ok "version field has no leading v" ;;
    *) bad "version field is not the bare 9.9.9" ;;
esac
case "$a" in
    *silo-v9.9.9-linux-amd64.tar.gz*) ok "asset names keep the v" ;;
    *) bad "asset URLs do not match the published asset names" ;;
esac
rm -rf "$T"

# --- the test block ----------------------------------------------------------
# `silo version` prints the bare version; asserting v#{version} fails for
# every release. This has been wrong in the tap's own copy.
sandbox
out=$(generate v9.9.9) || out=""
# Only the assertion itself, not the comment above it explaining why the
# leading v is wrong -- which of course contains the string being looked for.
assertion=$(printf '%s\n' "$out" | grep '^ *assert' || true)
case "$assertion" in
    *'v#{version}'*) bad "test block asserts a leading v that silo never prints" ;;
    "") bad "the formula has no test block at all" ;;
    *) ok "test block does not assert a leading v" ;;
esac
rm -rf "$T"

# --- failure modes -----------------------------------------------------------
# A missing sidecar must stop the run. Emitting a formula with an empty sha256
# would install an unverified binary.
sandbox
rm "$T/dist/silo-v9.9.9-linux-arm64.tar.gz.sha256"
generate v9.9.9 >/dev/null && status=0 || status=$?
[ "${status:-0}" != 0 ] && ok "refuses to generate with a sidecar missing" \
                        || bad "generated a formula despite a missing checksum"
rm -rf "$T"

# A truncated or non-hex checksum is corruption upstream of here, and a
# formula built from it installs something nobody checked.
sandbox
printf 'deadbeef  silo-v9.9.9-linux-amd64.tar.gz\n' \
    > "$T/dist/silo-v9.9.9-linux-amd64.tar.gz.sha256"
generate v9.9.9 >/dev/null && status=0 || status=$?
[ "${status:-0}" != 0 ] && ok "rejects a checksum that is not 64 hex chars" \
                        || bad "accepted a short checksum"
rm -rf "$T"

# No tag at all is a caller bug, not a default.
sandbox
generate >/dev/null && status=0 || status=$?
[ "${status:-0}" != 0 ] && ok "requires a version argument" \
                        || bad "generated a formula with no version"
rm -rf "$T"

# --- -o ----------------------------------------------------------------------
# CI writes straight into the tap checkout, so -o has to create the directory
# and write the same bytes that stdout would have carried.
sandbox
"$gen" v9.9.9 --dist "$T/dist" -o "$T/new/Formula/silo.rb" 2>/dev/null || true
if [ -f "$T/new/Formula/silo.rb" ]; then
    ok "-o creates the directory and writes the file"
    [ "$(cat "$T/new/Formula/silo.rb")" = "$(generate v9.9.9)" ] \
        && ok "-o writes what stdout would have" \
        || bad "-o output differs from stdout"
else
    bad "-o did not write the file"
fi
rm -rf "$T"

echo
if [ "$failures" -eq 0 ]; then
    echo "generate-formula.sh: all cases passed"
else
    printf 'generate-formula.sh: %d case(s) failed\n' "$failures"
    exit 1
fi
