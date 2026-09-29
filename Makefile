.PHONY: build install test test-race smoke fmt fmt-check vet glint check commit help

BINARY_NAME=graft
BUILD_DIR=bin
VERSION=$(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS=-ldflags "-X main.buildVersion=$(VERSION)"

all: build

build: ## Build the binary into bin/
	@mkdir -p $(BUILD_DIR)
	go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) .

install: build ## Install to ~/bin atomically (a running graft keeps its file)
	@mkdir -p $(HOME)/bin
	@set -eu; \
	tmp=$$(mktemp $(HOME)/bin/.$(BINARY_NAME).XXXXXX); \
	trap 'rm -f "$$tmp"' EXIT; \
	install -m 0755 $(BUILD_DIR)/$(BINARY_NAME) "$$tmp"; \
	mv -f "$$tmp" $(HOME)/bin/$(BINARY_NAME)
	@echo "Installed to $(HOME)/bin/$(BINARY_NAME)"

test: ## Run all tests (unit and integration)
	go test -count=1 ./...

test-race: ## Run all tests with the race detector
	go test -race -count=1 ./...

smoke: fmt-check vet test ## The commit gate

fmt: ## Format code
	gofmt -w .

fmt-check: ## Fail on unformatted code
	@test -z "$$(gofmt -l .)" || (echo "Not formatted, run make fmt:"; gofmt -l .; exit 1)

vet: ## Run go vet (host and Windows)
	go vet ./...
	GOOS=windows go vet ./...

glint: ## Run glint
	glint check ./...

check: smoke test-race glint ## Everything, before make install

commit: ## Commit through graft itself (MSG_FILE=<file> or M="subject")
	@if [ -n "$(MSG_FILE)" ]; then go run . commit -F "$(MSG_FILE)"; else echo 'Usage: go run . commit -m "..." (or -F file)'; exit 2; fi

help: ## Show targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'
