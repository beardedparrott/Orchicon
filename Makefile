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
COMPOSE     := docker compose
COMPOSE_FILE:= deploy/compose/docker-compose.yml
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
	$(BUF) generate

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

# --- Docker Compose dev stack ----------------------------------------------
.PHONY: up down logs ps nuke
up: ## Start the local dev stack (Postgres, NATS, SigNoz, OTel)
	$(COMPOSE) -f $(COMPOSE_FILE) up -d

down: ## Stop the local dev stack
	$(COMPOSE) -f $(COMPOSE_FILE) down

logs: ## Tail dev-stack logs
	$(COMPOSE) -f $(COMPOSE_FILE) logs -f --tail=100

ps: ## Show dev-stack status
	$(COMPOSE) -f $(COMPOSE_FILE) ps

nuke: ## Stop and DELETE all dev-stack data volumes
	$(COMPOSE) -f $(COMPOSE_FILE) down -v

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

# --- CI --------------------------------------------------------------------
.PHONY: ci
ci: lint gen vet test rls-check ## Run the full CI gate locally
