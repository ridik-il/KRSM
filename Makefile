.DEFAULT_GOAL := help
BIN := bin/krsm
PKG := ./...

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build the krsm binary
	go build -o $(BIN) ./cmd/krsm

.PHONY: test
test: ## Run tests with the race detector
	go test -race -count=1 $(PKG)

.PHONY: cover
cover: ## Run tests with coverage report
	go test -race -coverprofile=coverage.txt $(PKG)
	go tool cover -func=coverage.txt | tail -1

.PHONY: fmt
fmt: ## Format the code
	gofmt -s -w .

.PHONY: vet
vet: ## Run go vet
	go vet $(PKG)

.PHONY: lint
lint: ## Run golangci-lint (must be installed)
	golangci-lint run

.PHONY: staticcheck
staticcheck: ## Run staticcheck (must be installed)
	staticcheck $(PKG)

.PHONY: check
check: fmt vet lint staticcheck test ## The full local gate — mirrors CI (fmt, vet, lint, staticcheck, race tests)

.PHONY: snapshot
snapshot: ## Build a local release snapshot into dist/ (no publish/sign; needs goreleaser)
	goreleaser release --snapshot --clean

.PHONY: release-check
release-check: ## Validate .goreleaser.yaml without building (needs goreleaser)
	goreleaser check

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf bin dist coverage.txt coverage.html
