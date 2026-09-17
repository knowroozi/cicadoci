# CICaDo — root Makefile.
#
# Tooling is pinned in tools.go and installed into ./bin by `make dev-deps`;
# nothing here depends on what happens to be on your PATH.

SHELL       := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

BIN         := $(CURDIR)/bin
BUF         := $(BIN)/buf
GOLANGCILINT := $(BIN)/golangci-lint

# Build-time tooling lives in its own module (tools/go.mod) so that the root
# module's requirements describe what the shipped binaries actually link.
TOOLS_MOD := $(CURDIR)/tools

# Postgres connection used by integration tests. Point this at any database you
# like; `make test-e2e` starts a throwaway one instead if it is unset.
CICADO_TEST_DATABASE_URL ?=
export CICADO_TEST_DATABASE_URL

##@ Help

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nCICaDo development targets\n\nUsage:\n  make \033[36m<target>\033[0m\n"} \
		/^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 } \
		/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

##@ Setup

.PHONY: dev-deps
dev-deps: ## Install pinned tooling into ./bin
	@mkdir -p $(BIN)
	cd $(TOOLS_MOD) && GOBIN=$(BIN) go install \
		github.com/bufbuild/buf/cmd/buf \
		google.golang.org/protobuf/cmd/protoc-gen-go \
		google.golang.org/grpc/cmd/protoc-gen-go-grpc \
		github.com/golangci/golangci-lint/v2/cmd/golangci-lint
	@echo "tooling installed into $(BIN)"

##@ Development

.PHONY: dev
dev: ## Bring up server + Postgres + one runner (docker-compose)
	docker compose up --build

.PHONY: dev-down
dev-down: ## Tear down the dev stack and its volumes
	docker compose down -v

.PHONY: build
build: ## Build both binaries into ./bin
	go build -o $(BIN)/cicado-server ./server/cmd/cicado-server
	go build -o $(BIN)/cicado-runner ./runner/cmd/cicado-runner

##@ Protocol

.PHONY: proto
proto: ## Regenerate Go code from the frozen proto, then lint and breaking-check it
	$(BUF) generate
	$(BUF) lint
	@$(MAKE) --no-print-directory proto-breaking

.PHONY: proto-breaking
proto-breaking: ## Fail if the proto changed in a non-additive way vs. origin/master
	@if git rev-parse --verify --quiet origin/master >/dev/null; then \
		$(BUF) breaking --against '.git#branch=origin/master'; \
	else \
		echo "skipping breaking check: no origin/master to compare against"; \
	fi

.PHONY: generate-check
generate-check: ## Fail if committed generated code is stale
	@$(BUF) generate
	@if ! git diff --quiet -- proto/; then \
		echo "ERROR: generated proto code is out of date. Run 'make proto' and commit the result."; \
		git --no-pager diff --stat -- proto/; \
		exit 1; \
	fi
	@echo "generated code is up to date"

##@ Quality

.PHONY: test
test: ## Unit tests only (no external dependencies)
	go test ./... -short

.PHONY: test-all
test-all: ## Unit + integration tests (needs CICADO_TEST_DATABASE_URL)
	go test ./... -race

.PHONY: test-e2e
test-e2e: ## Walking skeleton end to end against a throwaway Postgres
	./scripts/with-postgres.sh go test ./test/e2e/... -race -count=1 -v

.PHONY: lint
lint: ## Run golangci-lint
	$(GOLANGCILINT) run ./...

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: tidy
tidy: ## Tidy both modules
	go mod tidy
	cd $(TOOLS_MOD) && go mod tidy

.PHONY: fmt
fmt: ## Format all Go code
	go fmt ./...

.PHONY: check-fast
check-fast: generate-check lint vet test ## Fast pre-push check: no Postgres, no -race
	@echo "check-fast passed. Run 'make check' before pushing for full coverage (starts a throwaway Postgres)."

.PHONY: check
check: generate-check lint vet ## Everything CI runs, minus the database-backed tests
	./scripts/with-postgres.sh go test ./... -race

.PHONY: clean
clean: ## Remove build output
	rm -rf $(BIN)/cicado-server $(BIN)/cicado-runner
