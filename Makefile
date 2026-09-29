.PHONY: build install test test-race smoke fmt fmt-check vet glint check commit help pg-up pg-down

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

# Tests that need PostgreSQL use this disposable container; without
# GRAFT_TEST_PG_DSN they skip and say so.
PG_CONTAINER=graft-test-pg
PG_PORT=55432
export GRAFT_TEST_PG_DSN=postgres://graft:graft@127.0.0.1:$(PG_PORT)/graft?sslmode=disable

pg-up: ## Start the disposable PostgreSQL for tests (docker)
	@if docker ps --format '{{.Names}}' | grep -qx $(PG_CONTAINER); then :; \
	elif docker ps -a --format '{{.Names}}' | grep -qx $(PG_CONTAINER); then docker start $(PG_CONTAINER) >/dev/null; \
	else docker run -d --name $(PG_CONTAINER) -p 127.0.0.1:$(PG_PORT):5432 \
		-e POSTGRES_USER=graft -e POSTGRES_PASSWORD=graft -e POSTGRES_DB=graft \
		postgres:18-alpine -c fsync=off -c synchronous_commit=off -c full_page_writes=off >/dev/null; fi
	@for i in $$(seq 1 60); do docker exec $(PG_CONTAINER) pg_isready -U graft -d graft >/dev/null 2>&1 && exit 0; sleep 1; done; \
		echo "$(PG_CONTAINER) not ready after 60s: docker logs $(PG_CONTAINER)"; exit 1

pg-down: ## Remove the test PostgreSQL container
	docker rm -f $(PG_CONTAINER)

test: pg-up ## Run all tests (unit, integration, PostgreSQL)
	go test -count=1 ./...

test-race: pg-up ## Run all tests with the race detector
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
