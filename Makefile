GO ?= go
GO_PACKAGES ?= ./...

.PHONY: check test test-e2e build docs-check docs-check-changed test-docs-check test-release-check

check:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		printf 'gofmt required for:\n%s\n' "$$unformatted"; \
		exit 1; \
	fi
	$(GO) vet $(GO_PACKAGES)

test:
	@packages="$$( $(GO) list $(GO_PACKAGES) | grep -v '/test/e2e$$')" && \
	$(GO) test -short -count=1 $$packages

test-e2e:
	$(GO) test -count=1 ./test/e2e

build:
	$(GO) build -o /dev/null ./cmd/9a

docs-check:
	./scripts/docs-check.sh all

docs-check-changed:
	./scripts/docs-check.sh changed

test-docs-check:
	./scripts/test-docs-check.sh

test-release-check:
	./scripts/test-release-check.sh
