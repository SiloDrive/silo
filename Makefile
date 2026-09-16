# Silo is a single Go binary; `go build ./...` is the build. This exists for the
# checks that are not `go test` and would otherwise only run when someone
# remembers them.

.PHONY: check test vet fmt check-refs vuln integration

# Everything CI should care about, in the order that fails fastest.
check: fmt vet test check-refs vuln

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

# The Ruby suite needs a running server, so it is not part of `check`.
# See test/README.md.
integration:
	@cd test && rake

# govulncheck exits non-zero only for a vulnerability your code actually
# reaches, so this gates on reachability rather than on the dependency list: an
# advisory in a module we require but never call is reported and does not fail
# the build. That is the distinction that makes it usable as a gate at all --
# x/crypto/openpgp is permanently on the module list with no fix, and a check
# that failed on it would be turned off within a week.
#
# Last: it is the only target here that wants the network, and there is no
# point resolving a vulnerability database for a tree that does not compile.
vuln:
	@command -v govulncheck >/dev/null 2>&1 || { \
		echo "govulncheck not found; go install golang.org/x/vuln/cmd/govulncheck@latest"; \
		exit 1; \
	}
	govulncheck ./...

# Docs here cite code by line number, which goes stale on the next edit above
# the cited line with nothing to catch it. See scripts/check-refs.sh.
check-refs:
	@./scripts/check-refs.sh
