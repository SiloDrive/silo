#!/usr/bin/env bash
#
# Builds the silo tarballs that go on a silodrive.io download page, and
# writes the manifest that page reads.
#
#   packaging/build-release.sh alpha
#   packaging/build-release.sh alpha linux/amd64 darwin/arm64
#
# This exists because the repository is private. Silo is AGPLv3 and the
# source will be public, but until it is there are no GitHub release assets
# anybody outside can fetch, and `brew install dkam/silo/silo` reaches a tap
# pointing at a 404. Somebody installing SiloDrive needs a server to point it
# at, so the server goes on the same page as the clients.
#
# It does not replace .github/workflows/build.yml, which is what a tagged
# release is. Both call `make build`, so the flags are defined once and a
# binary from here differs from one in a release only in which commit it came
# from. The parts this adds are the tarball naming, the manifest, and the
# channel directory.
#
# Then copy dist/<channel>/silo/ into that channel's directory on the web
# host — $ALPHA_DIR or $BETA_DIR — keeping the component directory:
#
#   rsync -av --exclude builds.json dist/alpha/silo/ \
#       web:/srv/silodrive/alpha/silo/
#   rsync -av dist/alpha/silo/builds.json \
#       web:/srv/silodrive/alpha/silo/
#
# Binaries first, manifest last: the page offers what the manifest names, so
# a manifest that arrives early advertises a download that 404s.
#
set -euo pipefail

cd "$(dirname "$0")/.."

# The directory this repository owns under a channel, and the name the site
# resolves in a download URL: /alpha/download/silo/<file>. It is declared in
# Release::Product in the silo-org site repository, and a name not on that
# list is a 404 — so changing it here means changing it there in the same
# breath.
component="silo"

# The channel is the first argument and there is no default, for the reason
# silo-drive-linux gives: defaulting means the day somebody cuts the first
# beta is the day they find out what the default was, and the failure is
# silent.
#
# Unlike silo-drive, nothing about the channel is stamped into this binary.
# There is nothing in it that expires and nothing in it that names a
# download page, so the channel here is only which directory the files go
# in — and a server that refused to serve on a date would be a server
# nobody should put their library on.
channel=${1-}
case "$channel" in
alpha | beta) shift ;;
*)
	echo "usage: $0 <alpha|beta> [platform...]" >&2
	echo "  the channel is the download page these tarballs go on, and there is no default" >&2
	exit 2
	;;
esac

# UTC, because the page prints this date to people in other places and a
# build cut at 9am in Melbourne is the day before in London.
built=$(date -u +%Y-%m-%d)

# git describe rather than a hand-edited constant, so the version somebody
# quotes in a bug report is a commit that can be checked out. --dirty
# because a build made from uncommitted work is one nobody else can
# reproduce, and that is worth knowing before spending a day on its bug
# report.
version=$(git describe --tags --always --dirty)

platforms=("$@")
if [ ${#platforms[@]} -eq 0 ]; then
	platforms=("$(go env GOOS)/$(go env GOARCH)")
fi

out="dist/${channel}/${component}"
rm -rf "$out"
mkdir -p "$out"

entries=()
for platform in "${platforms[@]}"; do
	os=${platform%%/*}
	arch=${platform##*/}
	base="silo-${version}-${os}-${arch}"

	# `make build` rather than a `go build` line of our own: the flags there
	# are not optional — CGO_ENABLED=0 in particular, without which a binary
	# built on a rolling-release distro refuses to start on an LTS — and a
	# second copy of them here would drift from the one CI uses.
	make --no-print-directory build \
		GOOS="$os" GOARCH="$arch" VERSION="$version" \
		OUT="${out}/${base}" >/dev/null

	# A tarball with a stable inner name, matching what a tagged release
	# publishes: `tar xf` gives you ./silo whichever platform you are on,
	# which is what install.sh and every set of instructions already assume.
	# The download is the tarball, so the checksum below is of the tarball —
	# it is what somebody can actually check against the page.
	tmp=$(mktemp -d)
	cp "${out}/${base}" "${tmp}/silo"
	tar -C "$tmp" -czf "${out}/${base}.tar.gz" silo
	rm -rf "$tmp" "${out}/${base}"

	file="${base}.tar.gz"
	bytes=$(stat -c %s "${out}/${file}")
	sha=$(sha256sum "${out}/${file}" | cut -d' ' -f1)

	# No expires_on. Silo is AGPLv3 and has no deadline: the field is absent
	# rather than set to something far away, because Release::Build reads an
	# absent date as "this build is perpetual" and prints so, where a date in
	# 2099 would be a promise the page repeats and nothing keeps.
	entries+=("$(printf '    {"platform": "%s", "version": "%s", "file": "%s", "built_on": "%s", "bytes": %s, "sha256": "%s"}' \
		"$platform" "$version" "$file" "$built" "$bytes" "$sha")")

	echo "${out}/${file}"
done

# builds.json is read by Release::Build in the site repo. The two describe
# one thing and have to be changed together. There is no channel field in
# it: the directory it sits in is which channel it is, and a second answer
# to that question is a second answer that can be wrong.
{
	echo '{'
	echo '  "builds": ['
	printf '%s' "${entries[0]}"
	for entry in "${entries[@]:1}"; do printf ',\n%s' "$entry"; done
	echo
	echo '  ]'
	echo '}'
} >"${out}/builds.json"

echo "${out}/builds.json"
echo
echo "${channel}/${component}: built ${built}, no expiry"
