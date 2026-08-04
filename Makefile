# Orchicon Makefile.
#
# Common targets for the control plane (Go) and frontend (Vite+React).
# Tooling (buf, atlas) is expected on PATH; `make tools` installs them
# via `go install`. See AGENTS.md for the dev workflow.

SHELL := /usr/bin/env bash
.DEFAULT_GOAL := help

# --- Paths -----------------------------------------------------------------
GO          := go
BUF         := buf
ATLAS       := atlas
NPX         := npx
DB_URL      ?= postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable
BIN_DIR     := bin

# Git metadata injected into the binary via -ldflags (internal/version).
GIT_COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
GIT_TAG     := $(shell git describe --tags --abbrev=0 2>/dev/null || echo dev)
BUILD_DATE  := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS     := -X github.com/beardedparrott/orchicon/internal/version.gitCommit=$(GIT_COMMIT) \
               -X github.com/beardedparrott/orchicon/internal/version.gitTag=$(GIT_TAG) \
               -X github.com/beardedparrott/orchicon/internal/version.buildDate=$(BUILD_DATE)

# --- Help ------------------------------------------------------------------
.PHONY: help
help: ## Show available targets
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_-]+:.*##/ {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

# --- Tooling ---------------------------------------------------------------
.PHONY: tools
tools: ## Install buf and atlas into $$GOPATH/bin
	$(GO) install github.com/bufbuild/buf/cmd/buf@latest
	@command -v $(ATLAS) >/dev/null 2>&1 || curl -sSfL https://atlasgo.sh | sh

# --- Codegen ---------------------------------------------------------------
.PHONY: gen lint proto
gen: ## Generate Go + TypeScript from the Protobuf schema (buf generate)
	PATH="$(CURDIR)/frontend/node_modules/.bin:$$PATH" $(BUF) generate

lint: ## Lint the Protobuf schema (buf lint)
	$(BUF) lint

proto: lint gen ## Lint + generate

# --- Go control plane ------------------------------------------------------
.PHONY: build run test vet tidy
build: ## Build the control-plane binary into bin/
	@mkdir -p $(BIN_DIR)
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/orchicon ./cmd/orchicon

run: ## Run the control plane from source
	$(GO) run -ldflags "$(LDFLAGS)" ./cmd/orchicon

test: ## Run Go tests
	$(GO) test ./...

vet: ## Run go vet
	$(GO) vet ./...

tidy: ## Run go mod tidy
	$(GO) mod tidy

# clean removes local build artifacts + the Go build cache. The Go cache
# grows to tens of GB during heavy dev (the compiler keeps every
# intermediate build artifact); Go auto-trims it lazily but rarely down to
# a small size. Run this when disk is tight — it does NOT touch the DB,
# container images, or any runtime data.
.PHONY: clean cache-check
clean: ## Remove local build artifacts and the Go build cache (dev hygiene)
	$(GO) clean -cache -testcache
	@command -v $(GO) >/dev/null 2>&1 && go clean -modcache 2>/dev/null || true
	@rm -f $(BIN_DIR)/orchicon

# cache-check reports the current Go build cache size so devs can decide
# whether to run `make clean` before a heavy session (AGENTS.md disk hygiene).
cache-check: ## Show the Go build cache size
	@echo "GOCACHE: $(shell $(GO) env GOCACHE)"
	@du -sh "$$($(GO) env GOCACHE)" 2>/dev/null | cut -f1 || echo "0B"

# --- Database --------------------------------------------------------------
.PHONY: migrate migrate-diff migrate-hash rls-check
migrate: ## Apply pending Atlas migrations to $$DB_URL
	cd db && $(ATLAS) migrate apply --env local --url "$(DB_URL)"

migrate-diff: ## Generate a new migration from db/schema.hcl (usage: make migrate-diff name=foo)
	@test -n "$(name)" || { echo "usage: make migrate-diff name=<migration_name>"; exit 1; }
	cd db && $(ATLAS) migrate diff $(name) --env local --to "file://schema.hcl" --dir "file://migrations"

migrate-hash: ## Recompute the Atlas migration directory hash (after hand-edits)
	cd db && $(ATLAS) migrate hash --dir "file://migrations"

rls-check: ## CI gate: every tenant_id table must have the RLS policy (docs/09 §8.5)
	scripts/check-rls.sh "$(DB_URL)"

# --- Frontend --------------------------------------------------------------
.PHONY: fe-install fe-dev fe-build fe-lint
fe-install: ## Install frontend dependencies
	cd frontend && npm install

fe-dev: ## Start the Vite dev server
	cd frontend && npm run dev

fe-build: ## Build the frontend for production
	cd frontend && npm run build

fe-lint: ## Lint the frontend
	cd frontend && npm run lint

# --- Single container (deployment) -----------------------------------------
# The single container is the only full-stack deployment (dev + prod as two
# instances on offset ports). See scripts/container.sh.
.PHONY: container-build container-rebuild container-up container-down container-status container-logs container-ps runtime-build runtime-daemon runtime-stop
container-build: ## Build bin/orchicon + the container image
	$(MAKE) build
	scripts/container.sh build
runtime-build: ## Build the workflow runtime base image
	$(MAKE) build
	scripts/container.sh build
runtime-daemon: ## Start the host-side workflow runtime daemon
	scripts/container.sh runtime-daemon
runtime-stop: ## Stop the host-side workflow runtime daemon
	scripts/container.sh runtime-stop
container-rebuild: ## Stop an instance, rebuild the image, start it (usage: make container-rebuild dev|prod)
	@test -n "$(instance)" || { echo "usage: make container-rebuild instance=dev|prod"; exit 1; }
	scripts/container.sh down $(instance)
	$(MAKE) container-build
	scripts/container.sh up $(instance)
container-up: ## Start the dev single-container instance
	scripts/container.sh up dev
container-down: ## Stop the dev single-container instance
	scripts/container.sh down dev
container-status: ## Show single-container instance status
	scripts/container.sh status
container-logs: ## Tail the dev container instance logs
	scripts/container.sh logs dev
container-ps: ## List orchicon container instances
	scripts/container.sh ps

# --- Install ---------------------------------------------------------------
.PHONY: install-dry-run install-uninstall
install-dry-run: ## Dry-run the install script (no changes made)
	scripts/install.sh --dry-run

install-uninstall: ## Uninstall Orchicon via the install script
	scripts/install.sh --uninstall

# --- CI --------------------------------------------------------------------
.PHONY: ci
ci: lint gen vet test rls-check ## Run the full CI gate locally
