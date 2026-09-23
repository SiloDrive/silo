# Silo is a single Go binary, and `go build ./...` is still the quick compile
# check. This file holds the two things that are not that: the deploy build,
# whose flags are not optional and are easy to forget, and the checks that are
# not `go test` and would otherwise only run when someone remembers them.

.PHONY: build check test vet fmt vuln check-refs check-install check-formula integration

# The deploy build: the same flags a release ships, so a binary built here and
# one downloaded from a release differ only in which commit they came from.
# .github/workflows/build.yml calls this target rather than repeating the
# flags, so there is one definition of what a Silo build is instead of two
# that drift apart.
#
# CGO_ENABLED=0 is not optional. With cgo, the net and os/user resolvers link
# against the build machine's libc, and a binary built on a rolling-release
# distro then requires a newer glibc than an Ubuntu LTS ships: it fails at
# exec with "GLIBC_2.34 not found" and no stack to look at. Disabling cgo
# makes the binary static and the question go away.
#
# The rest earn their place more quietly: -trimpath keeps the builder's home
# directory out of the embedded paths, -s -w drops ~8MB of symbol table and
# DWARF, and -X main.Version stamps the version so `silo version` reports the
# tree it was built from rather than the hardcoded default in cmd/silo/main.go.
#
# Override any of these: `make build GOARCH=arm64`, or OUT to write elsewhere.
VERSION ?= $(shell git describe --tags --always --dirty)
GOOS    ?= linux
GOARCH  ?= amd64
OUT     ?= silo
# How a binary built here arrives on a machine. The .deb, .rpm and AUR packages
# repackage this same binary, so they do not override it -- they install a
# marker at <prefix>/share/silo/install-method instead, which `silo upgrade`
# prefers. See internal/upgrade.
INSTALL_METHOD ?= tarball

build:
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -trimpath \
	  -ldflags "-s -w -X main.Version=$(VERSION) -X main.InstallMethod=$(INSTALL_METHOD)" -o "$(OUT)" ./cmd/silo

# Everything CI should care about, in the order that fails fastest.
check: fmt vet test vuln check-refs check-install check-formula

fmt:
	@out="$$(gofmt -l .)"; \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	go vet ./...

# -p 1 for the reason build.yml gives: several fileserver test packages share a
# fixed temp data directory with the same library ID, so running packages in
# parallel races them. Without it this target fails intermittently and CI does
# not, which is the worst way round -- it teaches you to re-run `make check`
# until it passes. Proper fix is t.TempDir() in those tests.
test:
	go test -p 1 ./...

# govulncheck is a tool dependency (see the tool directive in go.mod), so this
# runs the pinned version rather than whatever happens to be on the PATH, and
# needs no install step of its own.
#
# It reports on call reachability, not on presence: an advisory in a module we
# require but never call is printed and does not fail. That is the behaviour we
# want -- x/crypto/openpgp is unmaintained with no fix available and nothing
# here imports it, so failing on it would mean either a permanent red build or
# a suppression file nobody revisits.
#
# GO-2026-5970 is why this exists: an infinite loop in x/text, reachable from
# cmd/silo, sat in go.mod until an audit went looking. See silo#74.
vuln:
	go tool govulncheck ./...

# The Ruby suite needs a running server, so it is not part of `check`.
# See test/README.md.
integration:
	@cd test && rake

# Docs here cite code by line number, which goes stale on the next edit above
# the cited line with nothing to catch it. See scripts/check-refs.sh.
check-refs:
	@./scripts/check-refs.sh

# install.sh decides things -- whether a native package would be a better
# install, whether it may write over one a package manager owns -- and none of
# it is reachable from `go test`. See scripts/test-install.sh.
check-install:
	@./scripts/test-install.sh

# The Homebrew formula is generated here rather than in the tap, so a
# mistake in it is a mistake in this repository. See scripts/test-formula.sh.
check-formula:
	@./scripts/test-formula.sh
