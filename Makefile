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

.PHONY: hooks-install
hooks-install: ## Enable the repo's git pre-commit hook for this worktree (per-checkout, not per-clone-via-tracking)
	./scripts/install-hooks.sh

##@ Development

.PHONY: dev
dev: ## Bring up server + Postgres + one runner (docker-compose)
	docker compose up --build

.PHONY: dev-down
dev-down: ## Tear down the dev stack and its volumes
	docker compose down -v

.PHONY: dev-reset-runner
dev-reset-runner: ## Clear the runner's persisted state (recovers from a stale credential after a DB reset)
	docker compose rm -svf runner
	docker volume rm -f cicado_runner-state
	@echo "runner-state cleared; 'make dev' will re-enroll the runner from scratch"

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

# BASE_REF is overridable so CI can point the breaking check at something
# other than origin/master — e.g. the pre-push SHA on a push to master, where
# the checked-out tree already *is* origin/master and comparing it against
# itself would trivially pass rather than actually skip.
BASE_REF ?= origin/master

.PHONY: proto-breaking
proto-breaking: ## Fail if the proto changed in a non-additive way vs. BASE_REF (default origin/master)
	@if ! git rev-parse --verify --quiet $(BASE_REF) >/dev/null; then \
		echo "skipping breaking check: no $(BASE_REF) to compare against"; \
	elif ! git cat-file -e $(BASE_REF):buf.yaml 2>/dev/null; then \
		echo "skipping breaking check: $(BASE_REF) predates the buf module, so there is"; \
		echo "no comparable baseline. This self-heals the moment this lands on master."; \
	else \
		$(BUF) breaking --against ".git#ref=$(BASE_REF)"; \
	fi

.PHONY: generate-check
generate-check: check-proto-versions check-go-versions ## Fail if generated code is stale or build base < go.mod
	@$(BUF) generate
	@if ! git diff --quiet -- proto/ || [ -n "$$(git status --porcelain -- proto/)" ]; then \
		echo "ERROR: generated proto code is out of date. Run 'make proto' and commit the result."; \
		git --no-pager diff --stat -- proto/; \
		git status --porcelain -- proto/; \
		exit 1; \
	fi
	@echo "generated code is up to date"

.PHONY: check-proto-versions
check-proto-versions: ## Fail if root and tools disagree on the protobuf runtime version
	@root=$$(grep -m1 '^\s*google.golang.org/protobuf ' go.mod | awk '{print $$2}'); \
	tools=$$(grep -m1 '^\s*google.golang.org/protobuf ' $(TOOLS_MOD)/go.mod | awk '{print $$2}'); \
	if [ -z "$$root" ] || [ -z "$$tools" ]; then \
		echo "ERROR: could not find a google.golang.org/protobuf requirement in go.mod and/or tools/go.mod"; \
		exit 1; \
	elif [ "$$root" != "$$tools" ]; then \
		echo "ERROR: go.mod pins protobuf $$root but tools/go.mod (which builds protoc-gen-go) pins $$tools."; \
		echo "Committed generated code is emitted by the tools-module generator but linked against the"; \
		echo "root-module runtime; keep the two in step. Align them and run 'make tidy'."; \
		exit 1; \
	fi
	@echo "root and tools agree on protobuf $$(grep -m1 '^\s*google.golang.org/protobuf ' go.mod | awk '{print $$2}')"

.PHONY: check-go-versions
check-go-versions: ## Fail if the Dockerfile build base is older than go.mod's `go` directive (CIC-149)
	@go run ./scripts/go-versions/ go.mod Dockerfile

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

.PHONY: check-ua-staleness
check-ua-staleness: ## Report knowledge-graph (`.ua/`) freshness; warn-only, degrade gracefully on an unreachable recorded commit (CIC-86)
	@go run ./scripts/ua-staleness .

# In CI (CI=1) the same check is strict: a stale or unreachable graph fails the job.
.PHONY: check-ua-staleness-strict
check-ua-staleness-strict: ## Fails when the `.ua/` graph is stale or its recorded commit is unreachable (CIC-86)
	@CI=1 go run ./scripts/ua-staleness .

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
check: generate-check lint vet ## Everything CI runs, including the database-backed tests (starts a throwaway Postgres)
	./scripts/with-postgres.sh go test ./... -race

.PHONY: clean
clean: ## Remove build output
	rm -rf $(BIN)/cicado-server $(BIN)/cicado-runner
