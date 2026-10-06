# Development tasks. `make check` runs what the lint, test and build jobs run
# in CI; `make help` lists everything.
.DEFAULT_GOAL := help

BIN           := anyship
GOLANGCI_LINT ?= golangci-lint
# What `make dist` stamps into the binaries; the Release workflow passes the tag.
VERSION       ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0)-next
# The platforms release archives are built for (scripts/dist.sh).
PLATFORMS     := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

.PHONY: help check build build-all test vet fmt fmt-check lint tidy-check schema test-images e2e-vps dist release-notes clean

help: ## List the targets
	@awk 'BEGIN {FS = ":.*## "} /^[a-z0-9-]+:.*## / {printf "  %-12s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

check: fmt-check vet tidy-check lint test build ## Everything CI checks on Linux, before you push

build: ## Build ./anyship for this machine
	go build -o $(BIN) ./cmd/anyship

build-all: ## Compile every package for every release platform
	@for platform in $(PLATFORMS); do \
		echo "go build ($$platform)"; \
		GOOS=$${platform%/*} GOARCH=$${platform#*/} CGO_ENABLED=0 go build ./... || exit 1; \
	done

test: ## Unit tests with the race detector
	go test -race ./...

vet: ## go vet
	go vet ./...

fmt: ## Format the code
	gofmt -w .

fmt-check: ## Fail if a file needs gofmt
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then echo "needs gofmt:"; echo "$$unformatted"; exit 1; fi

lint: ## golangci-lint (the version CI uses is pinned in .github/workflows/ci.yml)
	$(GOLANGCI_LINT) run ./...

tidy-check: ## Fail if go.mod or go.sum is not tidy
	go mod tidy -diff

schema: ## Regenerate schema/anyship.schema.json after changing the spec
	go run ./cmd/anyship schema > schema/anyship.schema.json

test-images: ## Build and run an image for every sample app (needs Docker)
	ANYSHIP_DOCKER_TESTS=1 go test ./dockerfile -run TestGeneratedImagesServeHTTP -v -timeout 30m

e2e-vps: build ## Deploy a sample app to localhost over ssh and remove it (needs Docker and sshd)
	ANYSHIP=./$(BIN) scripts/e2e-vps.sh

dist: ## Build every release archive and checksums.txt into dist/
	scripts/dist.sh $(VERSION)

release-notes: ## Print the release notes for VERSION from CHANGELOG.md (make release-notes VERSION=0.3.0)
	@ALLOW_UNRELEASED=1 scripts/release-notes.sh $(VERSION)

clean: ## Remove build output
	rm -f $(BIN)
	rm -rf dist
