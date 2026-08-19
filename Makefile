# Silo is a single Go binary; `go build ./...` is the build. This exists for the
# checks that are not `go test` and would otherwise only run when someone
# remembers them.

.PHONY: check test vet fmt check-refs integration

# Everything CI should care about, in the order that fails fastest.
check: fmt vet test check-refs

fmt:
	@out="$$(gofmt -l .)"; \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	go vet ./...

test:
	go test ./...

# The Ruby suite needs a running server, so it is not part of `check`.
# See test/README.md.
integration:
	@cd test && rake

# Docs here cite code by line number, which goes stale on the next edit above
# the cited line with nothing to catch it. See scripts/check-refs.sh.
check-refs:
	@./scripts/check-refs.sh
