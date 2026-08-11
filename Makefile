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
BUILD_DATE  := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
# Version tag resolution. An explicit override wins (e.g. `make build VERSION=v0.1.183`);
# otherwise the nearest reachable tag is used. `git pull` does NOT fetch tags, and the
# auto-release workflow creates the canonical release tag on GitHub at merge time, so a
# stale local tag view would embed an older version (a rebuild could report v0.1.181 for
# merged v0.1.183 code). Recursive (`=` + `?=`) so the tag is resolved at recipe time,
# AFTER the fetch-tags prerequisite has synced the local tags.
VERSION ?= $(shell git describe --tags --abbrev=0 2>/dev/null || echo dev)
LDFLAGS     = -X github.com/beardedparrott/orchicon/internal/version.gitCommit=$(GIT_COMMIT) \
               -X github.com/beardedparrott/orchicon/internal/version.gitTag=$(VERSION) \
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
# fetch-tags syncs local tags with origin before a build. `git pull` does
# not fetch tags, but the auto-release workflow creates the canonical
# release tag on GitHub at merge time — without this, a local rebuild would
# embed a stale version (git describe falls back to the nearest tag the
# local repo already knows). Best-effort: offline builds fall back to local
# tags and never fail.
.PHONY: fetch-tags
fetch-tags:
	@git fetch --tags --quiet origin 2>/dev/null || true

.PHONY: build run test vet tidy
# build/run depend on fe-build because the binary embeds frontend/dist via
# go:embed (assets.go). Without it, a stale dist silently ships the previous
# UI — exactly how the Ask Orchicon full-viewport fix stayed invisible after
# a "rebuild". fe-build is stamp-checked, so an unchanged frontend adds no
# cost to the Go-only iteration loop.
build: fetch-tags fe-build ## Build the control-plane binary into bin/
	@mkdir -p $(BIN_DIR)
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/orchicon ./cmd/orchicon

run: fetch-tags fe-build ## Run the control plane from source
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
	# Stale copies of the binary dropped into the container/runtime build
	# contexts by older scripts. The runtime image no longer bakes the
	# binary (the daemon bind-mounts its own executable), so a leftover
	# deploy/runtime/orchicon would only bloat the build context — remove
	# both here so a heavy dev session never leaves them behind.
	@rm -f deploy/container/orchicon deploy/runtime/orchicon

# cache-check reports the current Go build cache size so devs can decide
# whether to run `make clean` before a heavy session (AGENTS.md disk hygiene).
cache-check: ## Show the Go build cache size
	@echo "GOCACHE: $(shell $(GO) env GOCACHE)"
	@du -sh "$$($(GO) env GOCACHE)" 2>/dev/null | cut -f1 || echo "0B"

# clean-docker reclaims disk from Docker build leftovers WITHOUT touching
# the running stateful instance containers (dev/prod), their data volumes,
# or the Postgres volumes that preserve instance data. Safe to run
# regularly during dev: dangling (untagged) images, stopped containers, and
# volumes not referenced by any container. Note this WILL remove orphaned
# anonymous volumes from old compose-era/test runs — it does NOT remove
# tagged images you might still want (e.g. the rocm/vllm images).
.PHONY: clean-docker
clean-docker: ## Prune dangling Docker images, stopped containers, and unused volumes
	@docker image prune -f --filter "dangling=true"
	@docker container prune -f
	@docker volume prune -f

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

# fe-build rebuilds the production bundle only when the frontend source is
# newer than the existing dist — repeated `make build` stays fast while any
# frontend edit is guaranteed to land in the next binary.
fe-build: ## Build the frontend for production (skipped when dist is up to date)
	@if [ -f frontend/dist/index.html ] && ! find frontend/src frontend/index.html frontend/vite.config.ts frontend/tailwind.config.js frontend/postcss.config.js frontend/components.json -newer frontend/dist/index.html -print -quit 2>/dev/null | grep -q .; then \
		echo "==> frontend bundle up to date"; \
	else \
		echo "==> building frontend bundle"; \
		cd frontend && npm run build; \
	fi

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
