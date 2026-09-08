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
# develop-bump + auto-release workflows create the canonical tags on GitHub at merge time
# (develop-bump: one v0.1.x tag per merge to develop; auto-release: the release tag when
# the human merges develop → main with the release label), so a stale local tag view would
# embed an older version (a rebuild could report v0.1.181 for merged v0.1.183 code).
# Recursive (`=` + `?=`) so the tag is resolved at recipe time, AFTER the fetch-tags
# prerequisite has synced the local tags.
VERSION ?= $(shell git describe --tags --abbrev=0 2>/dev/null || echo dev)
LDFLAGS     = -X github.com/beardedparrott/orchicon/internal/version.gitCommit=$(GIT_COMMIT) \
               -X github.com/beardedparrott/orchicon/internal/version.gitTag=$(VERSION) \
               -X github.com/beardedparrott/orchicon/internal/version.buildDate=$(BUILD_DATE)

# --- Help ------------------------------------------------------------------
.PHONY: help
help: ## Show available targets
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_-]+:.*##/ {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

# --- Tooling ---------------------------------------------------------------
# buf is pinned (version + SHA256) and installed into ./bin by `make tools`
# so codegen is reproducible everywhere: no `@latest` drift, no 80s
# compile-from-source in CI. Targets resolve buf at recipe time — ./bin/buf
# when present (CI, or any dev who ran `make tools`), else the PATH binary.
BUF_VERSION := 1.72.0
BUF_SHA256  := a9c6186cf6fcf062b247345e1b7b12c26f580c1b2a4bbf4d3fe080abf85ceee8
BUF_BIN     = $(if $(wildcard $(BIN_DIR)/buf),$(BIN_DIR)/buf,buf)

.PHONY: tools
tools: ## Install pinned buf v$(BUF_VERSION) into bin/ (SHA256-verified)
	@if [ "$$(uname -m)" != "x86_64" ]; then \
		echo "==> make tools: pinned buf download is x86_64-only; using buf from PATH"; \
	elif [ -x "$(BIN_DIR)/buf" ] && "$(BIN_DIR)/buf" --version 2>/dev/null | grep -q "$(BUF_VERSION)"; then \
		echo "==> buf $(BUF_VERSION) already installed"; \
	else \
		mkdir -p $(BIN_DIR); \
		curl -sSfL -o $(BIN_DIR)/buf.tgz "https://github.com/bufbuild/buf/releases/download/v$(BUF_VERSION)/buf-Linux-x86_64.tar.gz"; \
		echo "$(BUF_SHA256)  buf.tgz" | (cd $(BIN_DIR) && sha256sum -c -); \
		tar -xzf $(BIN_DIR)/buf.tgz -C $(BIN_DIR) --strip-components=2 buf/bin/buf; \
		rm -f $(BIN_DIR)/buf.tgz; \
		echo "==> installed $(BIN_DIR)/buf v$(BUF_VERSION)"; \
	fi

# --- Codegen ---------------------------------------------------------------
.PHONY: gen lint proto
gen: tools ## Generate Go + TypeScript from the Protobuf schema (buf generate)
	PATH="$(CURDIR)/frontend/node_modules/.bin:$$PATH" $(BUF_BIN) generate

lint: tools ## Lint the Protobuf schema (buf lint)
	$(BUF_BIN) lint

proto: lint gen ## Lint + generate

gen-check: ## CI drift gate: regenerate and fail on any diff in generated code
	$(MAKE) gen
	@git diff --exit-code -- api/gen frontend/src/api/gen \
		|| { echo "ERROR: generated code drifted from committed files. Run 'make gen' and commit."; exit 1; }

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
build: fetch-tags fe-build ## Build the control-plane + TUI client binaries into bin/
	@mkdir -p $(BIN_DIR)
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/orchicon ./cmd/orchicon
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/orch ./cmd/orch

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
	@rm -f $(BIN_DIR)/orch
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
.PHONY: migrate migrate-diff migrate-hash rls-check synth-data
migrate: ## Apply pending Atlas migrations to $$DB_URL
	@command -v $(ATLAS) >/dev/null 2>&1 || curl -sSfL https://atlasgo.sh | sh
	cd db && $(ATLAS) migrate apply --env local --url "$(DB_URL)"

migrate-diff: ## Generate a new migration from db/schema.hcl (usage: make migrate-diff name=foo)
	@test -n "$(name)" || { echo "usage: make migrate-diff name=<migration_name>"; exit 1; }
	cd db && $(ATLAS) migrate diff $(name) --env local --to "file://schema.hcl" --dir "file://migrations"

migrate-hash: ## Recompute the Atlas migration directory hash (after hand-edits)
	cd db && $(ATLAS) migrate hash --dir "file://migrations"

rls-check: ## CI gate: every tenant_id table must have the RLS policy (docs/09 §8.5)
	scripts/check-rls.sh "$(DB_URL)"

synth-data: ## CI gate: no synthesized data planes in non-test source (ADR-0010)
	scripts/check_no_synth_data.sh

# --- Frontend --------------------------------------------------------------
.PHONY: fe-install fe-dev fe-build fe-lint fe-test
fe-install: ## Install frontend dependencies
	cd frontend && npm install

fe-dev: ## Start the Vite dev server
	cd frontend && npm run dev

# fe-build rebuilds the production bundle only when the frontend source is
# newer than the existing dist — repeated `make build` stays fast while any
# frontend edit is guaranteed to land in the next binary. Set force-fe=1 to
# ALWAYS rebuild (used by container-rebuild so a rebuilt instance is
# guaranteed to reflect the current source — the stamp check silently ships a
# stale dist when the working tree is older than the last build, which is how
# frontend fixes have repeatedly gone "invisible after a rebuild").
force-fe ?= 0
fe-build: ## Build the frontend for production (skipped when dist is up to date; force-fe=1 to always rebuild)
	@if [ "$(force-fe)" = "1" ] || { [ ! -f frontend/dist/index.html ] || find frontend/src frontend/index.html frontend/vite.config.ts frontend/tailwind.config.js frontend/postcss.config.js frontend/components.json -newer frontend/dist/index.html -print -quit 2>/dev/null | grep -q . ; }; then \
		echo "==> building frontend bundle"; \
		cd frontend && npm run build; \
	else \
		echo "==> frontend bundle up to date"; \
	fi

fe-lint: ## Lint the frontend
	cd frontend && npm run lint

fe-test: ## Run frontend unit/component tests (vitest; Playwright specs live under test:snapshots/test:a11y/test:scope)
	cd frontend && npm test

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
	$(MAKE) container-build force-fe=1
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

# --- Full rebuild (one command) --------------------------------------------
# A single command that runs everything needed before/for an instance rebuild:
#   1. all checks/tests          (make ci:  lint gen vet test rls-check)
#   2. migration hash sync       (make migrate-hash — keeps db/migrations/atlas.sum
#                                 in sync so the Atlas CLI path stays happy)
#   3. frontend + binary + image (container-build force-fe=1 — the frontend is
#                                 built and embedded into the binary via go:embed)
#   4. stop/restart the instance (down then up; the container boots with
#                                 MigrateOnBoot=true, which applies any pending
#                                 embedded migrations — so step 2 is the repo
#                                 hash sync and step 4 surfaces the DB migration)
#
# The DB migration itself is applied by the container at boot (migrate.Run), so
# there is no separate `make migrate` needed here — running it against the
# instance's Postgres would conflict with the container-owned DB.
.PHONY: full-rebuild rebuild-dev rebuild-prod
full-rebuild: ## One command: all checks/tests + migrate-hash + image build + instance restart (usage: make full-rebuild instance=dev|prod)
	@test -n "$(instance)" || { echo "usage: make full-rebuild instance=dev|prod"; exit 1; }
	$(MAKE) ci
	$(MAKE) migrate-hash
	$(MAKE) container-rebuild instance=$(instance)

rebuild-dev: ## One command: full checks/tests + rebuild + restart the DEV instance
	$(MAKE) full-rebuild instance=dev
	$(MAKE) orch-launcher-dev

rebuild-prod: ## One command: full checks/tests + rebuild + restart the PROD instance
	$(MAKE) full-rebuild instance=prod
	$(MAKE) orch-launcher-prod

# --- Dual orch launchers ----------------------------------------------------
# Two orch clients on PATH: `orch` tracks bin/orch (dev vintage) and
# `orch-prod` is a snapshot COPY of bin/orch refreshed only by the prod
# path — so a prod-vintage client survives later `rebuild-dev` runs (the
# operator holds prod back when dev has breaking changes). Wrapper scripts
# orch-dev / orch-prod preset ORCHICON_URL per instance and carry a clearly
# marked ORCHICON_TOKEN line for the user to fill per instance (dev and
# prod have separate identity stores, so one config token cannot serve
# both). Wrappers are generated only if absent (never overwritten) and
# chmod 600.
LAUNCHER_DIR ?= $(HOME)/.local/bin
.PHONY: orch-launchers orch-launcher-dev orch-launcher-prod
orch-launchers: ## Install/refresh the dual orch launchers into ~/.local/bin (orch + orch-prod + wrappers)
	@test -f $(BIN_DIR)/orch || { echo "ERROR: $(BIN_DIR)/orch not found — run 'make build' first"; exit 1; }
	@mkdir -p $(LAUNCHER_DIR)
	# orch: dev-tracked symlink to bin/orch (refreshed on every dev rebuild).
	@ln -sfn "$(CURDIR)/$(BIN_DIR)/orch" "$(LAUNCHER_DIR)/orch"
	@echo "==> orch launcher: $(LAUNCHER_DIR)/orch -> bin/orch (dev vintage)"
	# orch-prod: snapshot COPY of the current bin/orch (moves only on prod rebuild).
	@cp -f "$(BIN_DIR)/orch" "$(LAUNCHER_DIR)/orch-prod"
	@chmod +x "$(LAUNCHER_DIR)/orch-prod"
	@echo "==> orch-prod launcher: $(LAUNCHER_DIR)/orch-prod (snapshot copy, prod vintage)"
	# Generate-if-absent wrapper scripts (never overwrite an existing file).
	@if [ ! -f "$(LAUNCHER_DIR)/orch-dev" ]; then \
		printf '#!/bin/sh\n# orch-dev: dev instance launcher (generated by make orch-launchers).\n# Fill in the token for the DEV instance below (dev and prod have separate\n# identity stores, so each instance needs its own token).\nexport ORCHICON_URL=http://localhost:8080\nexport ORCHICON_TOKEN=\nexec "$(LAUNCHER_DIR)/orch" "$$@"\n' > "$(LAUNCHER_DIR)/orch-dev"; \
		chmod 600 "$(LAUNCHER_DIR)/orch-dev"; \
		echo "==> generated $(LAUNCHER_DIR)/orch-dev (fill in ORCHICON_TOKEN)"; \
	else \
		echo "==> $(LAUNCHER_DIR)/orch-dev already exists — leaving untouched"; \
	fi
	@if [ ! -f "$(LAUNCHER_DIR)/orch-prod" ]; then \
		printf '#!/bin/sh\n# orch-prod: prod instance launcher (generated by make orch-launchers).\n# Fill in the token for the PROD instance below (dev and prod have separate\n# identity stores, so each instance needs its own token).\nexport ORCHICON_URL=http://localhost:8091\nexport ORCHICON_TOKEN=\nexec "$(LAUNCHER_DIR)/orch-prod" "$$@"\n' > "$(LAUNCHER_DIR)/orch-prod"; \
		chmod 600 "$(LAUNCHER_DIR)/orch-prod"; \
		echo "==> generated $(LAUNCHER_DIR)/orch-prod (fill in ORCHICON_TOKEN)"; \
	else \
		echo "==> $(LAUNCHER_DIR)/orch-prod already exists — leaving untouched"; \
	fi
	@echo ""
	@echo "  Tokens are per-instance (separate identity stores). Fill ORCHICON_TOKEN"
	@echo "  in $(LAUNCHER_DIR)/orch-dev and $(LAUNCHER_DIR)/orch-prod."

# orch-launcher-dev refreshes the dev-tracked launcher (orch symlink +
# orch-dev wrapper) after a dev rebuild.
orch-launcher-dev: ## Refresh the dev orch launcher after a dev rebuild
	@test -f $(BIN_DIR)/orch || { echo "ERROR: $(BIN_DIR)/orch not found — run 'make build' first"; exit 1; }
	@mkdir -p $(LAUNCHER_DIR)
	@ln -sfn "$(CURDIR)/$(BIN_DIR)/orch" "$(LAUNCHER_DIR)/orch"
	@echo "==> orch launcher refreshed: $(LAUNCHER_DIR)/orch -> bin/orch (dev vintage)"
	@if [ ! -f "$(LAUNCHER_DIR)/orch-dev" ]; then \
		printf '#!/bin/sh\n# orch-dev: dev instance launcher (generated by make orch-launchers).\n# Fill in the token for the DEV instance below (dev and prod have separate\n# identity stores, so each instance needs its own token).\nexport ORCHICON_URL=http://localhost:8080\nexport ORCHICON_TOKEN=\nexec "$(LAUNCHER_DIR)/orch" "$$@"\n' > "$(LAUNCHER_DIR)/orch-dev"; \
		chmod 600 "$(LAUNCHER_DIR)/orch-dev"; \
		echo "==> generated $(LAUNCHER_DIR)/orch-dev (fill in ORCHICON_TOKEN)"; \
	else \
		echo "==> $(LAUNCHER_DIR)/orch-dev already exists — leaving untouched"; \
	fi

# orch-launcher-prod refreshes the prod snapshot (orch-prod copy + wrapper)
# after a prod rebuild. The dev launcher is left untouched, so a prod-vintage
# client survives later rebuild-dev runs.
orch-launcher-prod: ## Refresh the prod orch launcher snapshot after a prod rebuild
	@test -f $(BIN_DIR)/orch || { echo "ERROR: $(BIN_DIR)/orch not found — run 'make build' first"; exit 1; }
	@mkdir -p $(LAUNCHER_DIR)
	@cp -f "$(BIN_DIR)/orch" "$(LAUNCHER_DIR)/orch-prod"
	@chmod +x "$(LAUNCHER_DIR)/orch-prod"
	@echo "==> orch-prod launcher refreshed: $(LAUNCHER_DIR)/orch-prod (snapshot copy, prod vintage)"
	@if [ ! -f "$(LAUNCHER_DIR)/orch-prod" ]; then \
		printf '#!/bin/sh\n# orch-prod: prod instance launcher (generated by make orch-launchers).\n# Fill in the token for the PROD instance below (dev and prod have separate\n# identity stores, so each instance needs its own token).\nexport ORCHICON_URL=http://localhost:8091\nexport ORCHICON_TOKEN=\nexec "$(LAUNCHER_DIR)/orch-prod" "$$@"\n' > "$(LAUNCHER_DIR)/orch-prod"; \
		chmod 600 "$(LAUNCHER_DIR)/orch-prod"; \
		echo "==> generated $(LAUNCHER_DIR)/orch-prod (fill in ORCHICON_TOKEN)"; \
	else \
		echo "==> $(LAUNCHER_DIR)/orch-prod already exists — leaving untouched"; \
	fi

# --- Install ---------------------------------------------------------------
.PHONY: install-dry-run install-uninstall
install-dry-run: ## Dry-run the install script (no changes made)
	scripts/install.sh --dry-run

install-uninstall: ## Uninstall Orchicon via the install script
	scripts/install.sh --uninstall

# --- CI --------------------------------------------------------------------
# The CI gate, split the same way .github/workflows/ci.yml splits it:
# ci-go is the Go control-plane gate (no full Node install — `gen` pulls
# only the two protoc plugin packages); fe-lint/fe-test are the frontend
# gate and run in the fe CI job. `ci` is the local convenience union.
.PHONY: ci ci-go
ci-go: lint gen-check vet test synth-data rls-check ## Run the Go control-plane CI gate (mirrors the go-ci workflow job)
ci: ci-go fe-lint fe-test ## Run the full CI gate locally (Go + frontend)
