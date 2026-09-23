#!/bin/sh
#
# Tests for install.sh.
#
# install.sh decides things now -- whether a native package would be a better
# install than a loose binary, whether it may write over one a package manager
# owns -- and none of that is reachable from `go test`. So it gets a harness.
#
# Each case runs the real script against a fake release served over file://,
# with PATH cut down to a directory this script builds. That last part is the
# point: `command -v dpkg` then answers what the test says rather than what the
# machine running the test happens to have, so the detection under test is the
# real one and not a mock of it.
#
# Requires curl (the file:// fetch) and the usual coreutils.

set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
failures=0

ok()   { printf '  ok    %s\n' "$*"; }
bad()  { printf '  FAIL  %s\n' "$*"; failures=$((failures + 1)); }

# --- sandbox ---------------------------------------------------------------
# A release with a tarball, its checksum and a .deb, and a PATH containing only
# what install.sh legitimately needs.
sandbox() {
    T=$(mktemp -d)
    # SILO_RELEASE_BASE has the tag appended before the asset name, so the
    # files live one directory down, exactly as a real release serves them.
    mkdir -p "$T/dl/v9.9.9" "$T/bin" "$T/dest"

    case "$(uname -m)" in
        x86_64|amd64)  goarch=amd64 ;;
        aarch64|arm64) goarch=arm64 ;;
        *) printf 'test-install.sh: unsupported test arch %s\n' "$(uname -m)" >&2; exit 2 ;;
    esac
    case "$(uname -s)" in
        Linux)  goos=linux  ;;
        Darwin) goos=darwin ;;
        *) printf 'test-install.sh: unsupported test os %s\n' "$(uname -s)" >&2; exit 2 ;;
    esac

    asset="silo-v9.9.9-${goos}-${goarch}.tar.gz"
    stage=$(mktemp -d)
    printf '#!/bin/sh\necho 9.9.9\n' > "$stage/silo"
    chmod 0755 "$stage/silo"
    tar -C "$stage" -czf "$T/dl/v9.9.9/$asset" silo
    rm -rf "$stage"
    ( cd "$T/dl/v9.9.9" && sha256sum "$asset" > "$asset.sha256" )

    cat > "$T/rel.json" <<JSON
{"tag_name":"v9.9.9",
 "html_url":"https://github.com/SiloDrive/silo/releases/tag/v9.9.9",
 "assets":[
   {"name":"$asset","browser_download_url":"file://$T/dl/v9.9.9/$asset"},
   {"name":"silo_9.9.9_${goarch}.deb","browser_download_url":"file://$T/dl/v9.9.9/silo_9.9.9_${goarch}.deb"},
   {"name":"silo_9.9.9_${goarch}.deb.sha256","browser_download_url":"file://$T/dl/x.sha256"}
 ]}
JSON

    # Only the tools the script is entitled to assume. Notably absent: dpkg,
    # rpm, pacman and brew, so a case that wants one adds it.
    for c in sh uname mktemp tar cat chmod mv mkdir rm id sed head cut tr \
             dirname basename curl sha256sum stat grep printf sleep gzip gunzip; do
        p=$(command -v "$c" 2>/dev/null) || continue
        ln -sf "$p" "$T/bin/$c"
    done
}

# run [script args...] -- captures combined output in $out, status in $status.
# env -i so the developer's own environment cannot decide a case.
run() {
    out=$(env -i \
        PATH="$T/bin" HOME="$T/home" \
        INSTALL_DIR="$T/dest" \
        SILO_LATEST_URL="file://$T/rel.json" \
        SILO_RELEASE_BASE="file://$T/dl" \
        SILO_INSTALL_MARKER="$T/no-such-marker" \
        sh "$root/install.sh" "$@" 2>&1) && status=0 || status=$?
}

contains()     { case "$out" in *"$1"*) return 0 ;; *) return 1 ;; esac; }

# --- cases -----------------------------------------------------------------

echo "install.sh"

# 1. Nothing native available: the binary install is the right one, and the
#    script should just do it with no ceremony.
sandbox
run
[ "$status" = 0 ]        && ok "installs when no package manager is present" \
                         || bad "exit $status with no package manager: $out"
[ -x "$T/dest/silo" ]    && ok "the binary lands in INSTALL_DIR" \
                         || bad "no binary at $T/dest/silo"
contains "dpkg"          && bad "mentions dpkg on a machine without it" \
                         || ok "says nothing about packages it cannot see"
rm -rf "$T"

# 2. dpkg present, no terminal to ask at. The .deb is the better install --
#    unit, service account, /etc/silo -- and the script must say so. What it
#    must NOT do is hang: there is no tty to read an answer from, and a script
#    that blocks forever inside `curl | sh` is worse than either choice.
sandbox
printf '#!/bin/sh\nexit 0\n' > "$T/bin/dpkg"; chmod 0755 "$T/bin/dpkg"
run
contains "silo_9.9.9"    && ok "names the .deb the release actually published" \
                         || bad "no .deb named in the notice: $out"
contains "systemd"       && ok "says what the .deb adds over a bare binary" \
                         || bad "notice does not explain the difference: $out"
[ "$status" = 0 ]        && ok "does not hang or fail without a terminal" \
                         || bad "exit $status with no tty: $out"
rm -rf "$T"

# 3. --force is the way past the notice for automation. It installs the binary
#    and does not editorialise.
sandbox
printf '#!/bin/sh\nexit 0\n' > "$T/bin/dpkg"; chmod 0755 "$T/bin/dpkg"
run --force
[ "$status" = 0 ]        && ok "--force installs" || bad "--force exit $status: $out"
[ -x "$T/dest/silo" ]    && ok "--force leaves the binary in place" \
                         || bad "--force installed nothing"
contains "Recommended"   && bad "--force still argues the case: $out" \
                         || ok "--force skips the recommendation"
rm -rf "$T"

# 4. --print is the auditable mode: say what would happen, touch nothing.
sandbox
run --print
[ "$status" = 0 ]        && ok "--print exits clean" || bad "--print exit $status: $out"
[ -e "$T/dest/silo" ]    && bad "--print installed a binary anyway" \
                         || ok "--print installs nothing"
contains "9.9.9"         && ok "--print names the version it would install" \
                         || bad "--print does not say what it would do: $out"
rm -rf "$T"

# 5. An unknown flag is a typo, and a typo that silently installs something is
#    the wrong outcome for a script people run as root.
sandbox
run --frce
[ "$status" = 2 ]        && ok "rejects an unknown flag" || bad "unknown flag exit $status: $out"
[ -e "$T/dest/silo" ]    && bad "installed despite the bad flag" \
                         || ok "installs nothing on a bad flag"
rm -rf "$T"

# 6. The package-manager guard from before, still holding: a marker means the
#    binary belongs to dpkg/rpm/pacman, and installing beside it puts a second
#    silo earlier on PATH than the one the service runs.
sandbox
printf 'deb\n' > "$T/marker"
out=$(env -i PATH="$T/bin" HOME="$T/home" INSTALL_DIR="$T/dest" \
        SILO_LATEST_URL="file://$T/rel.json" SILO_RELEASE_BASE="file://$T/dl" \
        SILO_INSTALL_MARKER="$T/marker" \
        sh "$root/install.sh" 2>&1) && status=0 || status=$?
[ "$status" != 0 ]       && ok "refuses to shadow a package-managed install" \
                         || bad "installed over a package-managed silo"
contains "silo upgrade"  && ok "points at the command that does it properly" \
                         || bad "refusal does not say what to do instead: $out"
rm -rf "$T"

echo
if [ "$failures" -eq 0 ]; then
    echo "install.sh: all cases passed"
else
    printf 'install.sh: %d case(s) failed\n' "$failures"
    exit 1
fi
