# Thin wrapper over the go toolchain. Nothing here is required -- every target is
# a one-liner you can run directly -- but it documents the canonical invocations
# and keeps CI and local runs using the same flags.

BINARY      := crossplane-postrender
BIN_DIR     := bin
MODULE      := github.com/jcogilvie/crossplane-helm-postrender
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo devel)
LDFLAGS     := -s -w -X $(MODULE)/internal/version.version=$(VERSION)

# The Docker network the render engine and function containers share. It must
# exist before a render: since crossplane CLI v2.4.0 the engine joins a network
# named by annotation rather than creating one.
NETWORK     ?= crossplane-render

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build the binary into ./bin
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY) ./cmd/$(BINARY)

.PHONY: test
test: ## Run tests with the race detector
	go test -race ./...

.PHONY: cover
cover: ## Run tests and report coverage
	go test -race -covermode=atomic -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

.PHONY: bench
bench: ## Run benchmarks
	go test ./internal/render/ -bench . -benchtime 5x -run '^$$'

.PHONY: lint
lint: ## Verify the lint config, then run golangci-lint
	# `config verify` first, because `run` is more permissive than the schema:
	# it accepts settings that CI's golangci-lint-action rejects outright.
	golangci-lint config verify
	golangci-lint run ./...

.PHONY: fmt
fmt: ## Format the code
	gofmt -w -s $(shell find . -name '*.go' -not -path './vendor/*')

.PHONY: tidy
tidy: ## Tidy go.mod/go.sum
	go mod tidy

.PHONY: network
network: ## Create the shared Docker network (idempotent)
	@docker network inspect $(NETWORK) >/dev/null 2>&1 \
		|| docker network create $(NETWORK)

.PHONY: reviewable
reviewable: tidy fmt lint test ## Everything CI checks, before opening a PR

.PHONY: clean
clean: ## Remove build and coverage output
	rm -rf $(BIN_DIR) dist coverage.out
