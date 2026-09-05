# ygo development makefile.
#
# Run `make check` before every commit. CI runs the same set; if
# `make check` is green locally, the PR will be green too (modulo
# go-version matrix differences and the cross-language fixtures job).

GO ?= go
GOLANGCI_LINT_VERSION ?= v2.12.2
GOLANGCI_LINT ?= $(shell command -v golangci-lint 2> /dev/null)

.PHONY: check
check: fmt-check vet test lint

.PHONY: fmt
fmt:
	gofmt -w .

.PHONY: fmt-check
fmt-check:
	@out=$$(gofmt -l .); \
	if [ -n "$$out" ]; then \
		echo "gofmt would reformat:"; \
		echo "$$out"; \
		exit 1; \
	fi

.PHONY: vet
vet:
	$(GO) vet ./...

.PHONY: test
test:
	$(GO) test -race -coverprofile=coverage.txt -covermode=atomic ./...

.PHONY: lint
lint:
ifndef GOLANGCI_LINT
	@echo "golangci-lint not found on PATH."
	@echo "Install matching CI version with:"
	@echo "  $(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)"
	@echo "(brew installs v2 by default; the project config is v1)"
	@exit 1
else
	$(GOLANGCI_LINT) run
endif

.PHONY: lint-install
lint-install:
	$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

.PHONY: fixtures
fixtures:
	cd testdata/gen && npm install --silent && node gen-lib0.mjs

# Run B1-B4 benchmarks. B4 skips unless testdata/b4-trace.json is
# present (run `make bench-fetch-b4` to download). Use
# `make bench BENCH=B1_1` to filter; pass `BENCHTIME=5x` for more
# samples.
BENCH ?= .
BENCHTIME ?= 3x
.PHONY: bench
bench:
	$(GO) test -bench=$(BENCH) -benchtime=$(BENCHTIME) -benchmem -timeout=900s ./benchmarks/

# Download the 3.2 MB upstream B4 editing trace. One-shot — gitignored.
.PHONY: bench-fetch-b4
bench-fetch-b4:
	cd testdata/gen && node fetch-b4-trace.mjs

.PHONY: clean
clean:
	rm -f coverage.txt coverage.html
	$(GO) clean ./...
