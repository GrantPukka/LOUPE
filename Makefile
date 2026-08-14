# loupe — see CONTRIBUTING.md for the full development setup.
#
# CGO is required: we link DuckDB. On Linux that means build-essential; on
# macOS, the Xcode command line tools. This is also why release builds need a
# native runner per platform rather than a cross-compile matrix.

GO      ?= go
BIN     ?= loupe
DIST    ?= dist
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

# The demo directory the blaster writes into, and that `make demo` opens.
DEMO ?= ./demo
SEED ?= 42

.DEFAULT_GOAL := help

## help: list the available targets
.PHONY: help
help:
	@grep -hE '^## ' $(MAKEFILE_LIST) | sed 's/## /  /' | sort

## build: build the loupe binary into the working directory
.PHONY: build
build:
	CGO_ENABLED=1 $(GO) build -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/loupe
	@echo "built $(BIN) $(VERSION) ($$(du -h $(BIN) | cut -f1))"

## install: build and install loupe into GOPATH/bin
.PHONY: install
install:
	CGO_ENABLED=1 $(GO) install -ldflags '$(LDFLAGS)' ./cmd/loupe

## blaster: build the fixture and demo log generator
.PHONY: blaster
blaster:
	CGO_ENABLED=0 $(GO) build -o blaster ./cmd/blaster

## test: run the full test suite
.PHONY: test
test:
	CGO_ENABLED=1 $(GO) test ./...

## test-update: regenerate every golden fixture, then review the diff
.PHONY: test-update
test-update:
	CGO_ENABLED=1 $(GO) test ./... -update
	@echo "golden files regenerated — read 'git diff testdata/' before committing"

## cover: run tests and open a coverage report
.PHONY: cover
cover:
	CGO_ENABLED=1 $(GO) test ./... -coverprofile=coverage.out
	$(GO) tool cover -func=coverage.out | tail -1

## bench: run benchmarks with allocation counts
.PHONY: bench
bench:
	CGO_ENABLED=1 $(GO) test ./... -run '^$$' -bench . -benchmem

## lint: gofmt, go vet, and golangci-lint
.PHONY: lint
lint: fmt-check
	$(GO) vet ./...
	@command -v golangci-lint >/dev/null 2>&1 \
		&& golangci-lint run \
		|| echo "golangci-lint not installed, skipping (see CONTRIBUTING.md)"

## fmt: format all Go source
.PHONY: fmt
fmt:
	gofmt -s -w $$(find . -name '*.go' -not -path './web/*')

.PHONY: fmt-check
fmt-check:
	@unformatted=$$(gofmt -s -l $$(find . -name '*.go' -not -path './web/*')); \
	if [ -n "$$unformatted" ]; then \
		echo "not gofmt-clean:"; echo "$$unformatted"; exit 1; \
	fi

## check: everything CI runs — format, vet, lint, and tests
.PHONY: check
check: lint test

## fixtures: regenerate the deterministic test fixtures in testdata/mixed
.PHONY: fixtures
fixtures:
	CGO_ENABLED=0 $(GO) run ./cmd/blaster -out ./testdata/mixed -seed 7 -duration 5m -malform 0.02

## demo: generate a fake incident and explore it
.PHONY: demo
demo: build
	CGO_ENABLED=0 $(GO) run ./cmd/blaster -out $(DEMO) -seed $(SEED) -scenario incident
	./$(BIN) $(DEMO)

## demo-ui: generate a fake incident and open it in the browser
.PHONY: demo-ui
demo-ui: build
	CGO_ENABLED=0 $(GO) run ./cmd/blaster -out $(DEMO) -seed $(SEED) -scenario incident
	./$(BIN) $(DEMO) --ui --open

## demo-follow: stream a live incident into $(DEMO) for testing --follow
.PHONY: demo-follow
demo-follow:
	CGO_ENABLED=0 $(GO) run ./cmd/blaster -out $(DEMO) -follow -rate 40

## web: build the frontend into internal/server/dist for embedding
.PHONY: web
web:
	cd web && npm install && npm run build

## ui: build the frontend and the binary, then open the demo in a browser
.PHONY: ui
ui: web build
	CGO_ENABLED=0 $(GO) run ./cmd/blaster -out $(DEMO) -seed $(SEED) -scenario incident
	./$(BIN) $(DEMO) --ui --open

## dist: release binary for the host platform (CGO forbids cross-compiling)
.PHONY: dist
dist:
	mkdir -p $(DIST)
	CGO_ENABLED=1 $(GO) build -trimpath -ldflags '$(LDFLAGS)' \
		-o $(DIST)/$(BIN)-$$($(GO) env GOOS)-$$($(GO) env GOARCH) ./cmd/loupe

## clean: remove build output, generated demo data, and caches
.PHONY: clean
clean:
	rm -rf $(BIN) blaster $(DIST) $(DEMO) coverage.out
	rm -rf internal/server/dist/assets internal/server/dist/index.html
	$(GO) clean -cache -testcache
