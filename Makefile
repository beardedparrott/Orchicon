# Orchicon Makefile.
#
# Common targets for the control plane (Go) and frontend (Vite+React).
# Tooling (buf, atlas) is expected on PATH; `make tools` installs them
# via `go install`. See AGENTS.md for the dev workflow.

SHELL := /usr/bin/env bash
.DEFAULT_GOAL := help

# --- Paths -----------------------------------------------------------------
# Go: prefer the PROJECT-LOCAL toolchain (.dev/tools) when it is provisioned.
# It lives inside the repo — NOT /tmp, which the container wipes on restart (its
# tmpfs) — so it survives, and using it needs no shell PATH setup. Falls back to
# a PATH `go` everywhere else (CI, fresh clones), so this is a no-op for anyone
# who has not run the project's tool bootstrap. Same prefer-local-else-PATH shape
# as BUF_BIN below.
#
# The GOPATH/GOCACHE/GOTMPDIR exports are not tidiness: with no GOPATH, go
# defaults to $HOME/go, and a root-owned $HOME fails outright with
# "mkdir /home/<user>/go: permission denied". Strict `?=` keeps an explicitly
# exported value winning.
#
# .dev/tools/go is the project's own Go, matching go.mod's requirement (verified
# 1.26.4). GOTOOLCHAIN stays `auto`, so a future go.mod bump resolves the newer
# toolchain from GOPATH without touching this file. `make toolchain` prints both.
DEV_TOOLS   := $(CURDIR)/.dev/tools
DEV_GO      := $(DEV_TOOLS)/go/bin/go
ifeq ($(wildcard $(DEV_GO)),)
GO          := go
else
GO          := $(DEV_GO)
GOPATH      ?= $(DEV_TOOLS)/gopath
export GOPATH
GOCACHE     ?= $(DEV_TOOLS)/gocache
export GOCACHE
GOTMPDIR    ?= $(DEV_TOOLS)/gotmp
export GOTMPDIR
# PATH as well: a couple of recipes call a bare `go` (the standing PTY gate), and
# they must resolve the SAME toolchain rather than whatever the shell happens to
# have. Deliberately NOT adding .dev/tools/bin — `buf` and `atlas` each resolve
# through their own prefer-bin-then-PATH rule (BUF_BIN / ATLAS_BIN, right below),
# and shadowing them here would change which codegen toolchain runs.
PATH        := $(DEV_TOOLS)/go/bin:$(PATH)
export PATH
endif
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
# ATLAS GETS THE SAME RULE, and its absence is what broke `make rebuild-dev`:
#
#     cd db && atlas migrate hash --dir "file://migrations"
#     bash: line 1: atlas: command not found
#
# The comment above the PATH block has always CLAIMED atlas resolved this way — "buf/atlas resolve
# through BUF_BIN's own prefer-bin-then-PATH rule" — but the rule was only ever written for buf, and
# `ATLAS := atlas` was a bare name that resolved through PATH alone. So the build depended on the
# operator's shell having atlas on PATH, which is exactly the fragility that bit: atlas lives in
# .dev/tools/bin, and the PATH entries that reached it were lost with the rest of the home directory.
#
# A BUILD MUST NOT DEPEND ON THE OPERATOR'S SHELL PROFILE — the profile is a file like any other, and
# this one was on the same filesystem an `rm` emptied. Prefer our own copies, then PATH.
ATLAS_BIN   = $(if $(wildcard $(BIN_DIR)/atlas),$(BIN_DIR)/atlas,$(if $(wildcard $(DEV_TOOLS)/bin/atlas),$(DEV_TOOLS)/bin/atlas,atlas))

.PHONY: toolchain
toolchain: ## Show the Go toolchain + env this Makefile will build with
	@echo "GO       = $(GO)"
	@echo "GOPATH   = $(GOPATH)"
	@echo "GOCACHE  = $(GOCACHE)"
	@echo "GOTMPDIR = $(GOTMPDIR)"
	@echo "--- the go command ---"
	@$(GO) version
	@echo "--- the toolchain a BUILD uses (go.mod: $(shell sed -n 's/^go //p' go.mod)) ---"
	@$(GO) env GOTOOLCHAIN

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
	@# A TEST MUST NEVER TOUCH THE DEVELOPER'S REAL CONFIG.
	@#
	@# One did: TestThemeCommand drives `/theme light` then `/theme dark`, and SetTheme persists the
	@# choice to ~/.orchicon/config — so `make test`, and therefore `make rebuild-dev` (ci → test),
	@# silently reset the operator's chosen theme to dark on EVERY REBUILD. They noticed ("everytime I
	@# rebuild it ALWAYS goes back to the blueish dark theme") and the cause was here, not in the build.
	@#
	@# The package redirects its own config dir too (internal/tui/isolate_test.go); this contains ANY
	@# package that writes config without asking, now or later. Declared at the command rather than
	@# exported into each test binary so there is no per-package opt-in to forget.
	@#
	@# A TEST MUST ALSO NEVER INHERIT THE OPERATOR'S LIVE PLANE. The same reasoning, one layer up: a
	@# shell that launched a host plane carries that plane's configuration (scripts/container.sh
	@# exports ORCHICON_SERVE_STATE_DIR, ORCHICON_GUARD_POLICY, ...), and a test that EXECS a
	@# subprocess hands the whole ambient environment to it. Four packages failed on the operator's
	@# machine and none in CI for exactly that reason — cmd/orchicon (the serve state paths resolved
	@# to the LIVE instance), internal/guard + internal/runtime (the shim ran the interactive profile
	@# where the tests assert the worker one), internal/claude (Ask's shim refused differently than
	@# the docs say).
	@#
	@# The list is testfixtures.AmbientConfigEnv, and each affected package unsets it in its own
	@# `init` (so a bare `go test ./...` works too, and a test written later cannot forget). It is
	@# cleared here as well so anything running under `make` matches CI even in a package that has no
	@# isolate file yet. It is NOT env -i: the opt-in variables (ORCHICON_TEST_DSN,
	@# ORCHICON_SKIP_NETWORK_TESTS, ORCHICON_LIVE_*) are deliberately left reachable.
	@for v in ORCHICON_GUARD_POLICY ORCHICON_GUARD_GRANTS ORCHICON_GUARD_ONCE ORCHICON_GUARD_PROJECT \
	         ORCHICON_GUARD_FULLSEND ORCHICON_SERVE_STATE_DIR; do unset "$$v"; done; \
	ORCHICON_CONFIG_DIR="$$(mktemp -d)" $(GO) test ./...

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

# clean-docker reclaims disk from ORCHICON's own Docker build leftovers, and nothing else.
#
# EVERY PRUNE HERE IS SCOPED TO WHAT THIS PROJECT OWNS, which the first version of this target was
# not. It ran `docker container prune -f` and `docker volume prune -f` — both HOST-WIDE. On a machine
# where the operator has any other Docker work, that removed THEIR stopped containers and THEIR
# unused volumes: `docker volume prune` deletes every unreferenced volume on the host, and Orchicon's
# own data is a BIND MOUNT (not a named volume), so it owned none of what it was deleting. A sweep for
# "anything that could damage another machine" found it: a destructive operation whose target was not
# anchored to what the program owns, which is the same defect class as the installer step that deleted
# the caller's own `bin/` and the guard test that deleted a home directory.
#
#   containers  SCOPED to our label. Every Orchicon container carries `orchicon-instance`, so the
#               filter removes ours and cannot reach anyone else's.
#   images      DANGLING only. A dangling image has no tag and no container referencing it, so it is
#               unreferenced by definition rather than by our guess. THIS IS STILL HOST-WIDE and is
#               the one clause that is not scoped — stated rather than glossed: the alternative is to
#               leave them, and an untagged image rebuilds for free.
#   volumes     GONE, deliberately. There is no filter that makes this ours: we create no labelled
#               volumes, so any predicate would be a guess about someone else's data. An operator who
#               wants a host-wide volume prune can run it themselves, knowing what it does.
.PHONY: clean-docker
clean-docker: ## Prune Orchicon's dangling images and stopped containers (never volumes)
	@docker image prune -f --filter "dangling=true"
	@docker container prune -f --filter "label=orchicon-instance"

# --- Database --------------------------------------------------------------
.PHONY: migrate migrate-diff migrate-hash rls-check synth-data
migrate: ## Apply pending Atlas migrations to $$DB_URL
	@command -v $(ATLAS_BIN) >/dev/null 2>&1 || curl -sSfL https://atlasgo.sh | sh
	cd db && $(ATLAS_BIN) migrate apply --env local --url "$(DB_URL)"

migrate-diff: ## Generate a new migration from db/schema.hcl (usage: make migrate-diff name=foo)
	@test -n "$(name)" || { echo "usage: make migrate-diff name=<migration_name>"; exit 1; }
	cd db && $(ATLAS_BIN) migrate diff $(name) --env local --to "file://schema.hcl" --dir "file://migrations"

migrate-hash: ## Recompute the Atlas migration directory hash (after hand-edits)
	@command -v $(ATLAS_BIN) >/dev/null 2>&1 || curl -sSfL https://atlasgo.sh | sh
	cd db && $(ATLAS_BIN) migrate hash --dir "file://migrations"

rls-check: ## CI gate: every tenant_id table must have the RLS policy (docs/09 §8.5)
	scripts/check-rls.sh "$(DB_URL)"

synth-data: ## CI gate: no synthesized data planes in non-test source (ADR-0010)
	scripts/check_no_synth_data.sh

adapter-bake-guard: ## CI gate: adapter CLIs are MOUNTED, never baked into image layers (ADR-0003/0005)
	go test ./internal/runtime/ -run 'TestAdapterCLINeverBaked' -count=1 -v

# --- Frontend --------------------------------------------------------------
.PHONY: fe-install fe-dev fe-build fe-lint fe-test docs-check
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

docs-check: ## Validate every Mermaid diagram in DOCUMENTATION.md with a real parser
	@# The prefix is REUSED once installed, so a repeat run is instant rather than re-resolving the
	@# tree every time; CI passes ORCHICON_MERMAID_PREFIX from its own $RUNNER_TEMP install.
	@if [ -n "$$ORCHICON_MERMAID_PREFIX" ]; then \
		node scripts/check-mermaid.mjs; \
	elif [ -d "$(CURDIR)/frontend/node_modules/mermaid" ]; then \
		node scripts/check-mermaid.mjs; \
	else \
		if [ ! -d "$(CURDIR)/.mermaid-check/node_modules/mermaid" ]; then \
			echo "==> installing the Mermaid parser into .mermaid-check (gitignored)"; \
			npm install --silent --no-audit --no-fund --prefix "$(CURDIR)/.mermaid-check" mermaid@10.9.8 jsdom; \
		fi; \
		ORCHICON_MERMAID_PREFIX="$(CURDIR)/.mermaid-check/node_modules" node scripts/check-mermaid.mjs; \
	fi

# --- Single container (deployment) -----------------------------------------
# The single container is the only full-stack deployment (dev + prod as two
# instances on offset ports). See scripts/container.sh.
.PHONY: container-build container-rebuild container-up container-down container-status container-logs container-ps runtime-build runtime-daemon runtime-stop
# Plane residency per rebuild: `host` (the container runs the SERVICES only and
# the plane runs on the HOST — now the product's default shape; see
# residency_for in scripts/container.sh, which resolves ${...:-host}) or
# `container` (the plane runs inside the instance's container, as it did before
# the host-residency migration).
#
# THIS VARIABLE IS NO LONGER THE PRODUCT'S DEFAULT, only this target's. It used
# to be described as "the launcher's own default stays container", and that
# stopped being true when the launcher's default moved to host — so
# `make container-rebuild` is now the one entry point that still produces the
# OLD shape. rebuild-dev/rebuild-prod override this per target (below).
residency = container
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
container-rebuild: ## Stop an instance, rebuild the image, start it (usage: make container-rebuild dev|prod [residency=host|container])
	@test -n "$(instance)" || { echo "usage: make container-rebuild instance=dev|prod"; exit 1; }
	# residency is passed down as ENV (per invocation) — never exported globally,
	# so rebuilding one instance cannot change the other's shape.
	ORCHICON_PLANE_RESIDENCY="$(residency)" scripts/container.sh down $(instance)
	$(MAKE) container-build force-fe=1
	ORCHICON_PLANE_RESIDENCY="$(residency)" scripts/container.sh up $(instance)
# Host-resident plane listeners: the plane binds its loopback address
# (ORCHICON_HTTP_ADDR → host clients: orch, the GUI) plus the docker bridge
# address at THIS instance's port, so its run containers can dial it. Both the
# bind (ORCHICON_HTTP_EXTRA_BIND) and the URL those containers are handed
# (ORCHICON_PLANE_PUBLIC_URL) come from the ONE place that computes them
# (`scripts/container.sh plane-bind <dev|prod>`, bridge_bind_env) and
# `plane-start` picks them up through container.sh's plane_env. Both are PER
# INSTANCE: never put a globally-shared ORCHICON_PLANE_PUBLIC_URL in a shell
# profile — a globally-set value points one instance's workers at the other's
# plane.
container-up: ## Start the dev single-container instance (ORCHICON_PLANE_RESIDENCY=host opt-in: plane on the host)
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
#   1. binaries                 (make build — bin/orchicon + bin/orch are built
#                                FIRST so every check/test exercises the code
#                               about to ship, never a stale binary: the
#                                 real-pty smoke gate executes bin/orch)
#   2. all checks/tests         (make ci:  lint gen vet test rls-check)
#   3. migration hash sync      (make migrate-hash — keeps db/migrations/atlas.sum
#                                 in sync so the Atlas CLI path stays happy)
#   4. frontend + binary + image (container-build force-fe=1 — the frontend is
#                                 rebuilt and embedded into the binary via go:embed)
#   5. stop/restart the instance (down then up; the container boots with
#                                 MigrateOnBoot=true, which applies any pending
#                                 embedded migrations — so step 3 is the repo
#                                 hash sync and step 5 surfaces the DB migration)
#
# The DB migration itself is applied by the container at boot (migrate.Run), so
# there is no separate `make migrate` needed here — running it against the
# instance's Postgres would conflict with the container-owned DB.
.PHONY: full-rebuild rebuild-dev rebuild-prod
# residency propagates to container-rebuild through the make chain: BOTH
# rebuild-dev and rebuild-prod pass residency=host explicitly, so each rebuild
# migrates its OWN instance to a host-resident plane and neither can alter the
# other's shape (the launcher's own default stays `container` — see
# residency_for in scripts/container.sh; nothing here is ever exported
# globally).
#
# `residency=container` on the command line OVERRIDES the target default (a
# command-line variable beats a target-specific one), which is the documented
# rollback: `make rebuild-prod residency=container` puts prod's plane back
# inside its container.
full-rebuild: ## One command: binary build + all checks/tests + migrate-hash + image build + instance restart (usage: make full-rebuild instance=dev|prod)
	@test -n "$(instance)" || { echo "usage: make full-rebuild instance=dev|prod"; exit 1; }
	$(MAKE) build
	$(MAKE) ci
	$(MAKE) migrate-hash
	$(MAKE) container-rebuild instance=$(instance)

rebuild-dev: residency = host
rebuild-dev: ## One command: full checks/tests + rebuild + restart the DEV instance (plane residency: host)
	$(MAKE) full-rebuild instance=dev residency=$(residency)
	$(MAKE) orch-launcher-dev

rebuild-prod: residency = host
rebuild-prod: ## One command: full checks/tests + rebuild + restart the PROD instance (plane residency: host; pass residency=container to keep it in its container)
	$(MAKE) full-rebuild instance=prod residency=$(residency)
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
# CROSS-PLATFORM COMPILE GATE. The platforms and flags are EXACTLY the release matrix's
# (release.yml: linux/darwin/windows on amd64+arm64, CGO_ENABLED=0, the two shipped binaries), because
# a gate that compiles a different set from the one shipped is a gate that can pass while the release
# fails — which is precisely what happened.
#
# WHY IT EXISTS. v0.4.0 was tagged and then produced NO release: release.yml died with
# `cmd/orchicon/serve.go:215:29: undefined: syscall.Kill` on both Windows targets. `syscall.Kill` is
# Unix-only and was called from a file with no build constraint, so windows/amd64 and windows/arm64
# could not compile — and nothing in CI noticed, because every gate ran on linux/amd64, where the
# symbol exists. `go build`, `go vet` and `go test` all pass on Linux for code that cannot build for
# Windows, so no amount of running them would have caught it. Compiling for the shipped platforms is
# the only check that does.
#
# Output goes to a temp directory: this is a COMPILE check, not a build, and dropping six pairs of
# binaries into the repo root would leave them behind.
CROSS_PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64
.PHONY: cross-compile
cross-compile: ## Compile the shipped binaries for every release platform (catches platform-specific breaks)
	@set -e; tmp="$$(mktemp -d)"; trap 'rm -rf "$$tmp"' EXIT; \
	for t in $(CROSS_PLATFORMS); do \
	  os="$${t%%/*}"; arch="$${t##*/}"; \
	  echo "==> $$os/$$arch"; \
	  GOOS="$$os" GOARCH="$$arch" CGO_ENABLED=0 $(GO) build -o "$$tmp/" ./cmd/orchicon ./cmd/orch; \
	done; \
	echo "==> all $(words $(CROSS_PLATFORMS)) release platforms compile"

ci-go: lint gen-check vet test synth-data rls-check adapter-bake-guard cross-compile ## Run the Go control-plane CI gate (mirrors the go-ci workflow job)
ci: ci-go fe-lint fe-test ## Run the full CI gate locally (Go + frontend)

.PHONY: tui-pty-gate
tui-pty-gate: ## Standing real-pty TUI verification gate (smoke + mouse + /connect)
	ORCH_PTY_SMOKE=1 go test ./internal/tui/ -run 'TestPTY' -count=1 -timeout 420s -v
