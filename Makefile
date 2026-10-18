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

# THE LOCAL GATE WALKS ONLY THIS MODULE'S PACKAGES — local scratch is excluded.
#
# tmp/ is the repo's gitignored scratch dir (agent worktrees, patch staging,
# one-off probe programs), and a scratch dir carries NO go.mod. A dir with no
# go.mod is not a module boundary, so `./...` WALKS STRAIGHT INTO IT and
# compiles whatever is lying there as part of this module.
#
# That is how a stray file broke the whole local gate:
#
#	tmp/installer-preserve/install_images_test.go:42:10: undefined: runtimeVariantRef
#	make: *** [Makefile:193: vet] Error 1
#
# The file was a stale copy of a test that had already MOVED into cmd/orchicon —
# it still called a symbol that moved with it, and there was no implementation
# beside it, so it could never compile. Nothing was wrong with the module, and
# `make rebuild-dev` could not run at all: `ci` runs vet and test over `./...`.
#
# CI NEVER SEES THIS, which is why it was invisible until a rebuild: the go-ci
# job checks out a CLEAN tree, and tmp/ is gitignored so it does not exist there.
# The failure is local-only and looks like a broken merge — so it belongs fixed
# here, once, rather than diagnosed again on the next rebuild.
#
# Stated as ONE variable used by every target that walks the tree, so a target
# added later cannot forget it. Any dir under tmp/ is skipped, including a
# worktree (those are skipped for a different reason — they DO have a go.mod).
#
# CROSS-COMPILE WAS THE TARGET THAT HAD NOT USED IT, and it is the one that proves the rule,
# because it is the only target that vets an EXPLICIT `./...` of its own. It was added after
# this variable and walked the tree directly, so a stray file under tmp/ broke `make
# rebuild-dev` at the CROSS-COMPILE step — "vet: tmp/.../restore.go: undefined: PolicyEvaluator"
# — while `make vet` and `make test` stayed GREEN, because they do consult this variable. Same
# class of scratch-vs-gate failure as the one above, in the one gate that skipped the guard.
# It now reads PKGS like the rest, so there is no longer a target that walks the tree without it.
#
# A FAILED `go list` IS NOT AN EMPTY ONE — and the guard below could not tell them apart.
# `2>/dev/null` threw away the ONLY line that explains a failure, so both cases produced an empty
# PKGS and the guard blamed the one cause it knew about: "no packages matched". That cost a full
# diagnosis of a HEALTHY tree. An agent had run with a RELATIVE GOMODCACHE — relative to the repo
# root, so the cache landed INSIDE the repo at tmp/orchicon/gomodcache — leaving ~1.2 GB of module
# sources under tmp/ with no go.mod. `go list ./...` then aborted the WHOLE pattern —
#
#	pattern ./...: directory tmp/orchicon/gomodcache/github.com/arbovm/levenshtein@...:
#	outside main module or its selected dependencies
#
# — and printed NOTHING on stdout, so `make vet` reported "no packages matched ./... (tmp/
# excluded)" over 98 perfectly good packages and `make rebuild-dev` could not run at all. The
# message invited the one conclusion that was FALSE ("the merge broke the module").
#
# So the failure is CAPTURED here and reported in `go`'s own words, while the empty-match text
# survives as the OTHER arm of the guard. Recursive (`=`, not `:=`) as before: PKGS is read inside
# recipes, and `:=` would run `go list` at PARSE time — in a worktree or on a fresh clone where the
# toolchain or the module graph is not ready yet. Cost: one extra `go list` per gate run, ~0.2s warm.
GO_LIST_ERR = $(shell $(GO) list ./... 2>&1 >/dev/null | head -3)
PKGS = $(shell $(GO) list ./... 2>/dev/null | grep -v '/tmp/')

BUF         := buf
ATLAS       := atlas
NPX         := npx

# THE INSTANCE THIS CHECKOUT'S DATABASE TARGETS. One word picks it:
# `make migrate instance=prod` uses prod's 5433, the default dev uses 5432. DB_URL
# is DERIVED from it through scripts/container.sh db-url — the same per-instance
# port table the launcher and `orchicon install` use — so the DSN and the bound
# port cannot drift and two instances on one host never collide (AC4).
#
# IT IS EMPTY WHEN IT CANNOT BE DERIVED, AND THAT IS THE POINT. It first shipped with
# a `|| echo <dev DSN>` fallback, which turned EVERY resolution failure into dev:
# `make migrate instance=staging` (a typo, or an instance the table does not define)
# printed no error and applied DEV's migrations to DEV's database, and
# `make rls-check instance=staging` then gated dev — the exact collision AC4 exists to
# prevent, reintroduced by a literal. So a failed resolution stays EMPTY and
# db_url_required refuses with the reason. Export DB_URL yourself to target a database
# this table does not describe.
instance    ?= dev
DB_URL      ?= $(shell scripts/container.sh db-url $(instance) 2>/dev/null)

# THE REFUSAL THAT REPLACES THE FALLBACK. Every target that OPENS $(DB_URL) runs this
# first, so "I do not know which instance you mean" is an error naming the instance and
# the remedy instead of a silent dev default. One definition, so the message cannot
# drift between migrate and rls-check.
db_url_required = @test -n "$(DB_URL)" || { echo "cannot resolve DB_URL for instance '$(instance)': scripts/container.sh db-url refused it." >&2; echo "  Use instance=<dev|prod|test> (the per-instance port table), or set DB_URL explicitly." >&2; exit 1; }

# ...BUT THE DEFAULT MUST NOT SILENCE THE TARGETS THAT REQUIRE A CHOICE.
#
# `instance ?= dev` is what makes `make run`/`migrate`/`services` work with no
# argument (AC1), and it is a DEFAULT, not a decision. Two targets deliberately
# REFUSE to run without an explicit instance — `instance-rebuild` and
# `full-rebuild` (DESTRUCTIVE: they stop an instance, rebuild its binaries and
# restart it) —
# and they test it with `test -n "$(instance)"`. A `?=` default makes that test
# see "dev" and pass, so `make full-rebuild` with no argument silently rebuilt
# and restarted DEV instead of printing its usage line: the guard is dead.
# That is not hypothetical drift, it is this variable's blast radius.
#
# `instance_given` is non-empty ONLY when the operator named the instance on the
# command line or in the environment — `$(origin)` is `file` for this default.
# The two guards test THIS, so a bare `make full-rebuild` refuses again while
# `make full-rebuild instance=dev` (and the `rebuild-*` wrappers, which pass
# `instance=` explicitly) still work.
instance_given = $(filter command line environment override,$(origin instance))
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
# `--match 'v[0-9]*'` KEEPS A MOVING PRERELEASE TAG OUT OF THE VERSION. `git describe --tags` accepts ANY
# tag, and with several lightweight tags on one commit the tie-break is LEXICAL — so on a commit the canary
# build had tagged, `canary` beat `v0.5.45` and a local rebuild reported "orchicon canary" for a tree that
# WAS v0.5.45. Releases are unaffected (CI takes the version from GITHUB_REF_NAME on a tag checkout); it
# bites local rebuilds, which is where it was noticed. Keep this match on every describe call site.
VERSION ?= $(shell git describe --tags --abbrev=0 --match 'v[0-9]*' 2>/dev/null || echo dev)
LDFLAGS     = -X github.com/beardedparrott/orchicon/internal/version.gitCommit=$(GIT_COMMIT) \
               -X github.com/beardedparrott/orchicon/internal/version.gitTag=$(VERSION) \
               -X github.com/beardedparrott/orchicon/internal/version.buildDate=$(BUILD_DATE)

# --- Help ------------------------------------------------------------------
.PHONY: help
help: ## Show available targets
	@# TARGET NAMES MAY CONTAIN DIGITS. The regex was [a-zA-Z_-]+, which silently dropped
	@# every target with a number in it — install-ps1-test was in the file with its `##`
	@# description and simply never printed, so the one list a developer trusts said the
	@# target did not exist. `make help` must list what is there.
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z0-9_-]+:.*##/ {printf "  \033[36m%-30s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

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

# ENGINE is the container engine every hygiene/teardown recipe drives (docker or
# podman). It resolves through scripts/lib/engine.sh — the ONE resolver the shell
# libs also use — so `make clean-docker` and `scripts/orchicon.sh uninstall`
# cannot disagree about which engine they are pruning with. The lib is SOURCED
# (it defines a function, it is not an executable), so make dots it into one
# POSIX shell and asks engine_cmd; `engine_available` is the same call. Recursive `=` on
# purpose: it resolves per invocation, so a newly installed podman is picked up
# without a fresh `make`. Empty when neither engine is installed, which
# clean-docker treats as a LOUD SKIP (hygiene must not fail a build).
# ORCHICON_ENGINE pins it (make ORCHICON_ENGINE=podman clean-docker, or the env var —
# make does not put a command-line variable in $(shell)'s environment, so it is checked
# as a make variable FIRST and the resolver second).
ENGINE      = $(if $(ORCHICON_ENGINE),$(ORCHICON_ENGINE),$(shell . scripts/lib/engine.sh 2>/dev/null && engine_cmd))

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

# run STARTS THIS INSTANCE'S LOCAL SERVICES FIRST (the `services` prerequisite),
# so it works on a host with no container engine and no Postgres installed:
# startHostServices re-execs this binary as `container` with host service
# residency, which downloads the pinned Postgres + NATS on first use
# (cmd/orchicon/binaries.go). The plane's OWN env is then pointed at the instance
# the services bound — the plane's config defaults are dev's 5432/4222
# (internal/config/config.go), so without these exports `instance=prod` would
# reach dev's database. ORCHICON_HTTP_ADDR is exported for the SAME reason, one
# layer up: the config default is a WILDCARD `:8080` (dev's port) on EVERY
# interface, so `instance=prod` would take dev's 8080 — the collision AC4
# forbids — and expose the plane on the LAN. The launcher binds this instance's
# own loopback port (scripts/container.sh plane_env), and so does this.
run: services fetch-tags fe-build ## Run the control plane from source (starts this instance's local services first)
	ORCHICON_INSTANCE=$(instance) \
	ORCHICON_DATA_DIR="$${ORCHICON_DATA_DIR:-$$HOME/.local/share/orchicon-$(instance)}" \
	ORCHICON_HTTP_ADDR="127.0.0.1:$$(scripts/container.sh services-port $(instance) control)" \
	ORCHICON_POSTGRES_DSN="$(DB_URL)" \
	ORCHICON_NATS_URL="nats://localhost:$$(scripts/container.sh services-port $(instance) nats)" \
	$(GO) run -ldflags "$(LDFLAGS)" ./cmd/orchicon

test: ## Run Go tests
	@test -n "$(PKGS)" || { if printf '%s\n' "$(GO_LIST_ERR)" | grep -q .; then printf 'ERROR: `go list ./...` FAILED — the local gate cannot run. This is NOT the empty-match case: tmp/ exclusion is not the cause, and the packages were never the problem.\n\n%s\n\nFix the cause above (a module cache or foreign module left in the repo scratch dirs is the usual one), then re-run.\n' "$(GO_LIST_ERR)" >&2; else printf 'ERROR: no packages matched ./... (tmp/ excluded) — refusing to run the gate over nothing\n' >&2; fi; exit 1; }
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
	@# SERVICES ARE DELIBERATELY NOT A PREREQUISITE OF test, and ORCHICON_SERVICE_RESIDENCY is
	@# deliberately NOT in the unset list below. CI runs this after `container up` against the container
	@# DB, so starting host services here would double-bind 5432; and a developer's inner-loop services
	@# (make services) must stay reachable, which is what the opt-in variables are for. The isolation
	@# that matters — config, live plane, guard — is unchanged.
	@for v in ORCHICON_GUARD_POLICY ORCHICON_GUARD_GRANTS ORCHICON_GUARD_ONCE ORCHICON_GUARD_PROJECT \
	         ORCHICON_GUARD_FULLSEND ORCHICON_SERVE_STATE_DIR; do unset "$$v"; done; \
	ORCHICON_CONFIG_DIR="$$(mktemp -d)" $(GO) test $(PKGS)

vet: ## Run go vet
	@test -n "$(PKGS)" || { if printf '%s\n' "$(GO_LIST_ERR)" | grep -q .; then printf 'ERROR: `go list ./...` FAILED — the local gate cannot run. This is NOT the empty-match case: tmp/ exclusion is not the cause, and the packages were never the problem.\n\n%s\n\nFix the cause above (a module cache or foreign module left in the repo scratch dirs is the usual one), then re-run.\n' "$(GO_LIST_ERR)" >&2; else printf 'ERROR: no packages matched ./... (tmp/ excluded) — refusing to run the gate over nothing\n' >&2; fi; exit 1; }
	$(GO) vet $(PKGS)

tidy: ## Run go mod tidy
	$(GO) mod tidy

# clean removes local build artifacts + the Go build cache. This is the HAMMER:
# `go clean -cache` empties the WHOLE cache, so the next build recompiles everything.
# For routine disk hygiene use `make cache-trim`, which bounds the cache by SIZE and keeps
# the entries the next build actually wants. Run `clean` when the cache is corrupt, or when
# you want a guaranteed-cold build (timings, or a suspected bad cache entry). It does NOT
# touch the DB, container images, or any runtime data.
.PHONY: clean cache-check cache-trim cache-trim-test
clean: ## Remove local build artifacts and the ENTIRE Go build cache (dev hygiene; see cache-trim)
	$(GO) clean -cache -testcache
	@command -v $(GO) >/dev/null 2>&1 && go clean -modcache 2>/dev/null || true
	@rm -f $(BIN_DIR)/orchicon
	@rm -f $(BIN_DIR)/orch
	@rm -f $(BIN_DIR)/atlas-ci
	# Stale copies of the binary that the hand-build recipe drops into a build
	# context (`cp bin/orchicon deploy/<context>/` — ARCHITECTURE.md's "build
	# the image by hand" steps). The runtime image no longer bakes the binary
	# (the daemon bind-mounts its own executable), so a leftover copy would only
	# bloat that context -- removed here so a heavy dev session never leaves one
	# behind. BOTH contexts are swept because both still exist and both accept a
	# copy; the line is a no-op for whichever one goes away first. Only
	# `orchicon` is ever copied in -- the TUI client (bin/orch) is not -- so
	# there is no second name to sweep.
	@rm -f deploy/runtime/orchicon deploy/container/orchicon

# cache-trim bounds the Go build cache by SIZE, evicting the coldest entries first.
#
# WHY NOT JUST RELY ON GO. The compiler keeps every intermediate artifact, so the cache grows
# to tens of GB during heavy dev — and Go's OWN trim cannot bring it back down. Its policy
# (cmd/go/internal/cache/cache.go) is age-based with a hard five-day floor:
#
#     trimLimit = 5 * 24 * time.Hour
#     cutoff := now.Add(-trimLimit - mtimeInterval)
#
# This repo's cache is never five days old. Measured at its worst: 71.6 GB across 99,869 entries
# with ZERO older than that cutoff, and a trim that had completed 50 minutes earlier reclaimed
# nothing at all. The comment that used to sit here said Go "auto-trims it lazily but rarely down
# to a small size" — true in form, false in effect: an age rule with a five-day floor cannot bound
# a cache that grows ~30 GB in a day.
#
# A SIZE cap is the only policy that bounds growth at any arrival rate. See
# scripts/gocache-trim.sh for the eviction semantics: coldest-first by mtime (which is what Go
# updates on reuse), with a grace window so a build running concurrently is never truncated.
cache-trim: ## Bound the Go build cache by size, evicting coldest-first (a no-op when under cap)
	@scripts/gocache-trim.sh "$(GOCACHE)" --cap-mb "$(CACHE_CAP_MB)" $(if $(filter 1,$(cache_dry)),--dry-run,)

# CACHE_CAP_MB is the ceiling `make cache-trim` enforces. 20 GiB is a little over the working set of
# an active dev loop on this repo (its hot native entries measured ~17 GB over 30h), so a healthy
# cache sits under it and a runaway one is pulled straight back down. Override per invocation:
#   make cache-trim CACHE_CAP_MB=8192
CACHE_CAP_MB ?= 20480

# cache-check reports the cache size AGAINST THE CAP, so the number that decides whether to trim is
# one command away. `make cache-trim cache_dry=1` previews the eviction without deleting anything.
#
# IT MEASURES WHAT THE TRIM BOUNDS — the ENTRY bytes — not `du -sb` of the whole tree. The two are
# close but not equal (du also counts the 256 subdirectories' metadata, trim.txt, and README), and
# that gap made this report "OVER CAP" immediately after a trim had converged: the trim had done its
# job and the check still demanded another run, forever. A gate that can never be satisfied is worse
# than no gate, so both sides now count the same bytes. Disk usage is still shown, because it is the
# number that matters for free space — it is just not the number the cap is expressed in.
cache-check: ## Show the Go build cache size against the trim cap
	@echo "GOCACHE: $(shell $(GO) env GOCACHE)"
	@cache="$$($(GO) env GOCACHE)"; \
	sz="$$(find "$$cache" -mindepth 2 -maxdepth 2 -type f \( -name '*-a' -o -name '*-d' \) -printf '%s\n' 2>/dev/null \
	      | awk '{t+=$$1} END{print t+0}')"; \
	on_disk="$$(du -sh "$$cache" 2>/dev/null | cut -f1)"; \
	cap="$(CACHE_CAP_MB)"; \
	echo "entries: $$(awk -v b="$$sz" 'BEGIN{split("B KiB MiB GiB TiB",u," "); i=1; while (b>=1024 && i<5){b/=1024;i++} printf (i==1?"%.0f %s":"%.1f %s"), b, u[i]}')   on disk: $${on_disk:-0B}   cap: $${cap} MiB"; \
	if [ "$$sz" -gt "$$((cap * 1024 * 1024))" ]; then \
	  echo "OVER CAP - run 'make cache-trim' (preview: make cache-trim cache_dry=1)"; \
	else \
	  echo "within cap"; \
	fi

# clean-docker reclaims disk from ORCHICON's own container-engine build leftovers, and nothing else.
#
# IT IS ENGINE-AWARE, NOT ENGINE-LOYAL. Every recipe line below drives $(ENGINE)
# (docker or podman, resolved by scripts/lib/engine.sh) because this project now
# runs on either. Podman's `container prune`/`image prune` are the same operations
# with the same filters, so the scoping story is unchanged -- but it has to be
# re-stated for podman, because the incident below is not a docker bug: it is a
# property of HOST-WIDE prunes, and podman has them too.
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
#
# PODMAN'S `volume prune` IS THE SAME HOST-WIDE OPERATION with the same absence of an ownership
# filter, so the refusal applies to both engines and is not a docker-only scruple. Do not "fix" this
# by adding a label filter to the volume prune: we do not label the volumes we create, so such a
# filter would either match nothing (a lie that looks like a fix) or match someone else's volume by
# accident. The one engine-level asymmetry worth knowing: podman also keeps build-cache and pod
# namespaces, and pruning those is likewise host-wide -- so they are not pruned here either.
.PHONY: clean-docker
clean-docker: ## Prune Orchicon's dangling images and stopped containers, never volumes (docker or podman)
	@test -n "$(ENGINE)" || { echo "==> no container engine found (docker/podman) — nothing to prune"; exit 0; }
	@$(ENGINE) image prune -f --filter "dangling=true"
	@$(ENGINE) container prune -f --filter "label=orchicon-instance"

# --- Database --------------------------------------------------------------
.PHONY: migrate migrate-diff migrate-hash rls-check synth-data
# APPLIED BY THE REPO'S OWN ENGINE, NOT THE ATLAS CLI. internal/migrate over the embedded
# db/migrations is what the plane applies on boot and what CI applies (tools/atlas-ci), so
# `make migrate` cannot disagree with either - the DSN resolved above is the only variable.
# The CLI path could not read this directory AT ALL: Atlas rejects the paired *_down.sql as a
# duplicate migration version, with the exact CLI the dev image installs (`curl -sSfL
# https://atlasgo.sh | sh`). It also wanted a network install and a matching CLI build, neither
# of which an offline, container-less inner loop has. migrate-diff / migrate-hash still use the
# CLI: they GENERATE and HASH the directory, which the Go engine deliberately does not do.
# BUILT, THEN RUN. `go run` would do the same compile, but its throwaway binary lands in the
# shared Go build cache, which some sandboxes refuse to exec from; bin/ is this repo's
# own artifact dir (gitignored) and `make clean` owns it.
migrate: ## Apply pending Atlas migrations to $$DB_URL (instance=<dev|prod|test> picks the instance)
	$(db_url_required)
	@$(GO) build -o $(BIN_DIR)/atlas-ci ./tools/atlas-ci
	DB_URL="$(DB_URL)" $(BIN_DIR)/atlas-ci

migrate-diff: ## Generate a new migration from db/schema.hcl (usage: make migrate-diff name=foo)
	@test -n "$(name)" || { echo "usage: make migrate-diff name=<migration_name>"; exit 1; }
	cd db && $(ATLAS_BIN) migrate diff $(name) --env local --to "file://schema.hcl" --dir "file://migrations"

migrate-hash: ## Recompute the Atlas migration directory hash (after hand-edits)
	@command -v $(ATLAS_BIN) >/dev/null 2>&1 || curl -sSfL https://atlasgo.sh | sh
	cd db && $(ATLAS_BIN) migrate hash --dir "file://migrations"

rls-check: ## CI gate: every tenant_id table must have the RLS policy (docs/09 §8.5)
	@# ORCHICON_INSTANCE travels WITH the DSN: check-rls.sh resolves the instance's OWN
	@# downloaded psql from it, so `rls-check instance=prod` must not read dev's record.
	$(db_url_required)
	ORCHICON_INSTANCE=$(instance) scripts/check-rls.sh "$(DB_URL)"

# --- Local services (the de-containerized inner loop) ----------------------
#
# On a host with NO container engine these are the backend `make run` and
# `make migrate` talk to. `orchicon services up` reuses the SAME host-services
# supervisor the installer uses (startHostServices → `orchicon container` with
# ORCHICON_SERVICE_RESIDENCY=host), so there is one implementation of "run the
# services as this user's processes". Only `run` depends on it: `migrate` and
# `rls-check` run in CI AFTER `container up` against the container DB, so a
# `services` prerequisite there would double-bind 5432.
.PHONY: services services-down services-status
services: ## Start this instance's local Postgres + NATS on the host (no container engine needed)
	ORCHICON_TELEMETRY=none $(GO) run -ldflags "$(LDFLAGS)" ./cmd/orchicon services up $(instance)

services-down: ## Stop this instance's local services and verify the ports are free
	$(GO) run -ldflags "$(LDFLAGS)" ./cmd/orchicon services down $(instance)

services-status: ## Show this instance's local services state and its DSN
	$(GO) run -ldflags "$(LDFLAGS)" ./cmd/orchicon services status $(instance)

synth-data: ## CI gate: no synthesized data planes in non-test source (ADR-0010)
	scripts/check_no_synth_data.sh

adapter-bake-guard: ## CI gate: adapter CLIs are MOUNTED, never baked into image layers (ADR-0003/0005)
	go test ./internal/runtime/ -run 'TestAdapterCLINeverBaked' -count=1 -v

# cache-trim-test pins the invariants of scripts/gocache-trim.sh. It belongs in ci-go because that
# script DELETES FILES: its failure mode is silent and destructive (the wrong entries go, the
# machine is mysteriously slow afterwards), and it runs UNATTENDED inside `make full-rebuild`. The
# harness fakes the cache, so it needs no real one and touches none.
cache-trim-test: ## CI gate: assert the Go cache trim's eviction semantics and its refusals
	bash scripts/tests/gocache-trim/run.sh

# disk-report answers a question `df` structurally cannot: how much of the filesystem is held by
# SNAPSHOTS rather than by live files. The two disagree by design, and the gap is invisible —
# deleting files frees nothing while a snapshot still references the extents. Measured on this
# machine: 65.6 GB deleted against 7.0 GB freed, with ~58 GB of it sitting in a snapshot, and
# 343 GiB (67% of all data in use) pinned in total. That is what this reports.
#
# WHY IT NEEDS ITS OWN TARGET. Every other hygiene target here measures FILES. This one measures
# the space where files are not — which is why `du` and `df` both looked healthy while the disk
# sat at 93%. It is a HOST DIAGNOSTIC, so it describes the machine it runs on and is deliberately
# never called from CI. disk-report-test is the part that IS a gate: it fakes the inputs and
# pins the subtraction, because a report that overstates the pinned figure argues the wrong way
# at exactly the moment someone is deciding whether to delete a snapshot.
DISK_REPORT_TARGET ?=
.PHONY: disk-report disk-report-test
disk-report: ## Report how much of the filesystem is held by btrfs snapshots, not live files
	@scripts/disk-report.sh $(DISK_REPORT_TARGET)

disk-report-test: ## CI gate: assert disk-report's subtraction, its bounds, and its exit codes
	bash scripts/tests/disk-report/run.sh
# install-ps1-test pins the Windows installer's two invisibles, both of which are silent
# by construction: it must never call `exit` (under `irm … | iex` the file's text runs in
# the operator's OWN session, so an exit closes the window and takes the diagnosis with
# it) and its long or interactive steps must STREAM rather than capture (a captured step
# is indistinguishable from a hang, and a captured PROMPT blocks for ever).
#
# It needs pwsh + iconv and no Windows at all: a wsl.exe double carries the contract, and
# the failure paths are run inside a child pwsh. PWSH=/path/to/pwsh overrides the lookup.
# This is pwsh 7, NOT Windows PowerShell 5.1 — the 5.1-only behaviours it must avoid (the
# `2>&1` ErrorRecord trap) are documented in the installer's source instead.
.PHONY: install-ps1-test
install-ps1-test: ## CI gate: the Windows installer never closes the session, and streams
	PWSH="$${PWSH:-$$(command -v pwsh || ls -d .dev/tools/pwsh/*/pwsh 2>/dev/null | head -1)}" \
		bash scripts/tests/install-ps1-wsl-encoding/run.sh

# install-path-test RUNS THE REAL INSTALLER against a real artifact: scripts/tests/install-path
# packages a build in the release's exact asset shape, serves it over HTTP, and runs scripts/install.sh
# against it end to end. It needs no network, no Docker and no release — which is the point, because the
# install path was previously exercised only by a user, and a broken installer reached two published
# releases that way. It covers the POSIX installer; install.ps1's WSL path needs a Windows runner (its
# offline behaviour is covered by install-ps1-test).
install-path-test: ## Run the real installer against a locally-served artifact, and the uninstall verbs against a stubbed docker
	bash scripts/tests/install-path/run.sh
	bash scripts/tests/install-path/uninstall.sh

# host-residency-test guards the LAUNCH SHAPE of an instance: which of the two residencies a plain
# `up` produces, that each instance's ports and plane URL are its own (a shared value points one
# instance's workers at another's database), and that the host plane's two listeners cannot collide.
#
# IT GUARDS TWO IMPLEMENTATIONS. The bash half drives container.sh's own functions (the launcher a dev
# checkout uses); the GO half drives `go test ./cmd/orchicon` against install_host.go (the binary a
# curl-installed host has — container.sh is NOT in the release archive). The two must agree, and the Go half
# is the one that matters for an installed host, so it is asserted rather than assumed.
#
# IT IS DOCKER-FREE, which is why it belongs in this gate rather than behind a container job: the bash half
# resolves shapes through container.sh's own functions with a stubbed log and no engine, the bridge is PINNED
# (ORCHICON_DOCKER_BRIDGE_IP) rather than discovered, and the Go half asserts the no-engine shape explicitly.
#
# IT IS ALSO THE HARNESS THAT WOULD HAVE CAUGHT 5cc8039c'S FALLOUT. That commit deliberately changed
# the default residency and did not update this harness, which then failed for weeks — unnoticed,
# because nothing ran it. A harness that is never run is a comment with a PASS line in it.
#
# The one thing it cannot check is the deleted-exe detector's behaviour unless it can copy a real
# binary into the repo's gitignored tmp/, so a restricted environment SKIPS that case LOUDLY and
# counts it in the summary rather than passing quietly.
.PHONY: host-residency-test
host-residency-test: ## CI gate: the instance launch shape (residency default, per-instance ports, plane binds)
	bash scripts/tests/host-residency/run.sh

# orchicon-cli-test is the gate for scripts/orchicon.sh — the command a developer
# runs every day. It needs no container engine, no database and no running plane:
# every delegation is asserted against stub binaries on PATH, so it runs anywhere
# CI runs. It covers what the script alone can get wrong — a truncated `help` (the
# usage block used to be a positional slice of the file's own header), a verb that
# stops reaching the CLI's shared control surface, a `status` that reports health it
# never verified, two instances that stop sharing nothing, a rebuild whose warning
# about not running the tests goes missing, and any surviving reference to the
# retired container launcher.
.PHONY: orchicon-cli-test
orchicon-cli-test: ## CI gate: scripts/orchicon.sh (verbs, help completeness, delegation, honest status)
	bash scripts/tests/orchicon-cli/run.sh

# local-services-test pins the INNER LOOP: that DB_URL is DERIVED per instance,
# that the pure query verbs honour overrides, that `services` is a prerequisite
# of `run` alone, and that the `test` isolation contract is unbroken. Offline —
# no Docker and no database.
.PHONY: local-services-test
local-services-test: ## CI gate: the local-services inner loop (derived DB_URL, per-instance ports, help rows)
	bash scripts/tests/local-services/run.sh

# no-engine-e2e-assertions-test pins the acceptance harness's OWN HONESTY: an
# assertion that reads a captured file can never PASS on an empty capture (that
# is how AC3 was once reported verified on a plane serving no UI at all). Offline
# — it sources the harness's functions and touches no engine, port or database.
.PHONY: no-engine-e2e-assertions-test
no-engine-e2e-assertions-test: ## CI gate: the no-engine acceptance harness never PASSes on an empty capture
	bash scripts/tests/no-engine-e2e-assertions/run.sh

# runtime-e2e-assertions-test pins the RUNTIME-CONTAINER acceptance harness's own
# honesty. That procedure is ABSENCE-SHAPED ("no container was created", "the gate
# rejected it", "the pooled container was reused"), and every one of those is
# trivially true of a `podman ps` that failed to run — so an ungated assertion
# would manufacture the very blindness the feature removes. Offline: it sources
# the harness's functions and touches no engine, port or database.
.PHONY: runtime-e2e-assertions-test
runtime-e2e-assertions-test: ## CI gate: the runtime-container acceptance harness never PASSes on an empty capture
	bash scripts/tests/runtime-e2e-assertions/run.sh

# --- Frontend --------------------------------------------------------------
.PHONY: fe-install fe-dev fe-build fe-lint fe-test docs-check site-check
fe-install: ## Install frontend dependencies
	cd frontend && npm install

fe-dev: ## Start the Vite dev server
	cd frontend && npm run dev

# fe-build rebuilds the production bundle only when the frontend source is
# newer than the existing dist — repeated `make build` stays fast while any
# frontend edit is guaranteed to land in the next binary. Set force-fe=1 to
# ALWAYS rebuild (used by instance-rebuild so a rebuilt instance is
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

# fe-typecheck is the ONLY thing that typechecks the frontend, and it exists because
# nothing did. `fe-lint` is eslint (it has no type information) and `fe-test` is vitest
# (transpile-only: types are erased), so a reference to a member that does not exist
# lints clean and tests clean. The only other place `tsc -b` ran was `npm run build`,
# which no gate invoked — so a type error could only ever be discovered by whoever next
# ran `make build`, i.e. the first developer to try a rebuild.
#
# THAT IS EXACTLY WHAT HAPPENED. #736 shipped two calls to
# RuntimeImageStatus.RUNTIME_IMAGE_STATUS_ABSENT — the generated TS member is `ABSENT`,
# because protoc-gen-es strips the enum-name prefix — onto develop. Every one of the
# seven CI checks stayed green while `make build` (and therefore `make rebuild-dev`) was
# broken for everyone, on a clean checkout.
#
# IT IS SEPARATE FROM fe-build ON PURPOSE: both tsconfigs are noEmit and the build info
# goes to node_modules/.tmp, so this emits no bundle, does not touch frontend/dist, and
# leaves the working tree clean — which is what makes it safe to put in the gate.
fe-typecheck: ## CI gate: typecheck the frontend (tsc -b) — the only gate that can see a type error
	cd frontend && npm run typecheck

# --- Desktop app -----------------------------------------------------------
# THE DESKTOP APP IS NOT PART OF CROSS_PLATFORMS. It is CGO + a native webview
# (WebKitGTK/WKWebView/WebView2), so it cannot go in the CGO_ENABLED=0
# cross-compile loop — that loop would fail, and folding it in would break
# `make cross-compile` for everyone. Each OS builds its own binary on its own
# runner in .github/workflows/release.yml.
.PHONY: desktop-deps desktop-build desktop-test desktop-test-live
desktop-deps: ## Print the host prerequisites for building the desktop app (wails CLI + webkit2gtk)
	@echo "desktop app prerequisites:"
	@echo "  Go toolchain (go.mod's version), Node 22+, CGO enabled"
	@echo "  Linux:   libgtk-3-dev libwebkit2gtk-4.1-dev   (apt-get install ...)"
	@echo "           the 4.1 ABI needs the webkit2_41 BUILD TAG (desktop-build sets it);"
	@echo "           only an old distro with libwebkit2gtk-4.0-dev drops it"
	@echo "  macOS:   Xcode command line tools (WKWebView is part of the OS)"
	@echo "  Windows: WebView2 runtime + gcc (either 32/64-bit match, per Wails docs)"
	@echo "  wails CLI: go install github.com/wailsapp/wails/v2/cmd/wails@v2.17.0"

# desktop-build builds the app for THIS OS only. The frontend build comes FIRST
# because `//go:embed all:frontend/dist` matches the tracked .gitkeep when the
# bundle has not been built: a misordered build compiles perfectly and opens an
# empty window.
# `webkit2_41` selects the WebKitGTK 4.1 ABI, which is what current distros ship as
# libwebkit2gtk-4.1-dev; without the tag the cgo files ask pkg-config for 4.0 and the
# build stops. Inert on macOS (WKWebView) and Windows (WebView2).
desktop-build: fe-build ## Build the desktop app for THIS OS (CGO on; NOT part of cross-compile)
	CGO_ENABLED=1 go build -tags desktop,production,webkit2_41 -o bin/orchicon-desktop ./cmd/orchicon-desktop

desktop-test: ## CI gate: the desktop shell's guards (no webview needed)
	go build ./internal/desktop/ ./internal/controlport/
	go test ./internal/desktop/... ./internal/controlport/...
	go vet ./internal/desktop/... ./internal/controlport/...

# desktop-test-live is the TRANSPORT PROOF: unary + server-streaming RPCs through
# the shell's own origin against a real plane. Skips loudly when the plane URL is
# unset, so a unit run reports "not configured" rather than a false green.
desktop-test-live: ## Transport proof through the shell origin against a REAL plane (set ORCHICON_DESKTOP_PLANE_URL)
	go test -tags desktop_e2e ./internal/desktop/ -run 'TestLive' -v

site-check: ## Assert the landing page's nav and version-stamp invariants
	@# The landing page is served from a CDN and rendered by a browser, so a CSS or
	@# markup defect in it has no other detector. This reads the committed page as
	@# TEXT and asserts the invariants (labels never split or shrunk, nine links not
	@# all inline, the footer version is a build-time stamp) — deliberately
	@# dependency-free, because no browser can run in the CI container.
	bash scripts/tests/site-nav-layout/run.sh

# version-tag-match — every version resolution must restrict itself to version-shaped tags.
#
# `canary` is a real tag and it MOVES (the canary build re-points it at develop's tip), so a bare
# `git describe --tags` can resolve a local rebuild's version to "canary" instead of the version it is —
# observed as {"version":"canary"} from a tree whose content was v0.5.45. Releases are unaffected (CI takes
# the version from GITHUB_REF_NAME on a tag checkout), which is exactly why nothing caught it: only local
# rebuilds and the site/notes scripts ever resolve it this way.
.PHONY: version-tag-match
version-tag-match: ## Assert no version resolution accepts a moving tag (canary)
	bash scripts/tests/version-tag-match/run.sh

docs-check: ## Validate every Mermaid diagram in the root docs with a real parser
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

# --- Instances (host-resident dev loop) -------------------------------------
# The control plane runs on THIS host; `scripts/orchicon.sh` starts, stops and
# reports it (its verbs: start|stop|restart|status|logs). Nothing here builds a
# container image, and nothing here talks to a container engine.
#
# THE SHAPE IS NO LONGER A CHOICE, SO IT IS NO LONGER A VARIABLE — and that is the
# lesson kept from the `residency` variable this section used to carry. That
# variable existed because ONE entry point (`container-rebuild`) shipped its own
# default while every other path took the launcher's, so a single target silently
# produced a different shape from a fresh install and from `make rebuild-dev`. A
# per-target default that disagrees with the product default is not an option, it
# is a divergence with a friendly name. If a future knob makes one path differ from
# the rest, the path is the bug — fix the path, do not add the knob.
#
# The plane's per-instance listeners (loopback ORCHICON_HTTP_ADDR plus the
# docker-bridge address at THIS instance's port, so runtime containers can dial
# it) are computed in ONE place — the launcher — and are PER INSTANCE. Never put a
# globally-shared ORCHICON_PLANE_PUBLIC_URL in a shell profile: a globally-set
# value points one instance's workers at the other's plane. This file re-derives
# none of it.
#
# Instance selection: `instance=dev` (the default) names which stack these targets
# act on. `instance_given` is the narrower question "did the operator actually say
# so", and only the DESTRUCTIVE targets ask it.
.PHONY: instance-build instance-up instance-down instance-restart instance-status instance-logs instance-ps instance-rebuild

# The launcher owns instance lifecycle; this file owns the Makefile concerns
# (build, guards, the rebuild chain). Keeping that split means the launcher can be
# rewritten — it is, to delegate to `orchicon instance` — without a Makefile edit.
INSTANCE_SH := scripts/orchicon.sh

instance-build: ## Build the binaries an instance runs (bin/orchicon + bin/orch)
	$(MAKE) build

instance-up: ## Start this instance on the host (services + plane; instance=dev default)
	@test -x $(BIN_DIR)/orchicon || { echo "ERROR: $(BIN_DIR)/orchicon not found — run 'make instance-build' first"; exit 1; }
	@$(INSTANCE_SH) start $(instance)

instance-down: ## Stop this instance on the host (plane and the services it started)
	@$(INSTANCE_SH) stop $(instance)

instance-restart: ## Restart this instance without rebuilding anything
	@$(INSTANCE_SH) restart $(instance)

instance-status: ## Show whether this instance's plane and services are up (and healthy)
	@$(INSTANCE_SH) status $(instance)

instance-logs: ## Tail this instance's plane log
	@$(INSTANCE_SH) logs $(instance)

instance-ps: ## List the ORCHICON instances on this host and their state
	@$(INSTANCE_SH) status

# DESTRUCTIVE: it stops the instance before it starts it, so it requires the
# operator to NAME the instance. The guard tests instance_given, not instance —
# `instance ?= dev` makes a bare `test -n "$(instance)"` see "dev" and pass, which
# is how a bare `make full-rebuild` used to silently rebuild dev.
instance-rebuild: ## Stop this instance, rebuild the binaries, start it (usage: make instance-rebuild instance=dev|prod|test)
	@test -n "$(instance_given)" || { echo "usage: make instance-rebuild instance=dev|prod|test"; exit 1; }
	$(MAKE) instance-down instance=$(instance)
	$(MAKE) instance-build force-fe=1
	$(MAKE) instance-up instance=$(instance)

# --- Plane lifecycle (ONE local mechanism, no API path) ----------------------
# These are the PLANE-only half of instance-up/instance-down: they leave the
# instance's host services exactly as they are, which is what a restart onto a
# freshly built binary needs.
#
# EVERY PATH HERE IS THE SAME LOCAL MECHANISM the TUI, the desktop app and the
# launcher call — `orchicon plane ...`, implemented once in
# internal/planecontrol. There is no lifecycle RPC and this file composes none:
# a browser cannot spawn a process, which is the only reason the API path ever
# existed, and the browser is not a lifecycle surface. Anything that needs a
# remote control is a separate decision, not a target here.
.PHONY: plane-status plane-start plane-stop plane-restart
plane-status: ## Show this instance's plane: PID, serving BINARY, drift, health
	@test -x $(BIN_DIR)/orchicon || { echo "ERROR: $(BIN_DIR)/orchicon not found — run 'make instance-build' first"; exit 1; }
	@$(BIN_DIR)/orchicon plane status $(instance)
plane-start: ## Start this instance's plane only (idempotent; leaves its services alone)
	@test -x $(BIN_DIR)/orchicon || { echo "ERROR: $(BIN_DIR)/orchicon not found — run 'make instance-build' first"; exit 1; }
	@$(BIN_DIR)/orchicon plane start $(instance)
plane-stop: ## Stop this instance's plane only (graceful, bounded, verified)
	@test -x $(BIN_DIR)/orchicon || { echo "ERROR: $(BIN_DIR)/orchicon not found — nothing to stop"; exit 1; }
	@$(BIN_DIR)/orchicon plane stop $(instance)
plane-restart: ## Restart this instance's plane so it re-reads its environment
	@test -x $(BIN_DIR)/orchicon || { echo "ERROR: $(BIN_DIR)/orchicon not found — run 'make instance-build' first"; exit 1; }
	@$(BIN_DIR)/orchicon plane restart $(instance)
# --- Runtime image + daemon (podman; no instance lifecycle) ------------------
# `runtime.sh` owns both: the version-gated build of the three stock runtime
# images, and the host daemon's lifecycle. They used to live in container.sh.
# The orchicon binary is NOT baked into these images — the daemon bind-mounts its
# own executable into every runtime container — so runtime-build does not depend
# on `make build` and a binary rebuild alone never needs an image rebuild.
RUNTIME_SH := scripts/runtime.sh
.PHONY: runtime-build runtime-daemon runtime-stop runtime-status
runtime-build: ## Build the workflow runtime base images with podman (version-gated)
	@$(RUNTIME_SH) build
runtime-daemon: ## Start the host-side workflow runtime daemon
	@$(RUNTIME_SH) daemon-start
runtime-stop: ## Stop the host-side workflow runtime daemon
	@$(RUNTIME_SH) daemon-stop
runtime-status: ## Show whether the runtime daemon is answering, and its socket
	@$(RUNTIME_SH) daemon-status

# --- Deprecated names (kept because muscle memory is the asset) --------------
# These have NO `## ` comment on purpose: `make help` is generated from those, and
# eight deprecated rows would bury the real list. Each alias says what it became
# when you run it, so the rename is discoverable from the command itself rather
# than from a doc you have to think to open.
#
# THE ALIAS MUST NOT DEFEAT THE DESTRUCTIVE GUARD. `$(MAKE) instance-rebuild
# instance=$(instance)` would put the dev DEFAULT on the sub-make's command line,
# where instance_given sees origin=command line and the refusal disappears. So the
# alias forwards instance= only when the operator named one.
.PHONY: container-build container-up container-down container-rebuild container-status container-logs container-ps
container-build:
	@echo "container-build is now instance-build (binaries only — there is no image to build)"
	@$(MAKE) instance-build
container-up:
	@echo "container-up is now instance-up (the instance is a HOST process, not a container)"
	@$(MAKE) instance-up instance=$(instance)
container-down:
	@echo "container-down is now instance-down"
	@$(MAKE) instance-down instance=$(instance)
container-rebuild:
	@echo "container-rebuild is now instance-rebuild (no image build; the plane runs on the host)"
	@$(MAKE) instance-rebuild $(if $(instance_given),instance=$(instance))
container-status:
	@echo "container-status is now instance-status"
	@$(MAKE) instance-status instance=$(instance)
container-logs:
	@echo "container-logs is now instance-logs"
	@$(MAKE) instance-logs instance=$(instance)
container-ps:
	@echo "container-ps is now instance-ps"
	@$(MAKE) instance-ps

# --- Plane lifecycle — see "Instances (host-resident dev loop)" above --------
# plane-status / plane-start / plane-stop / plane-restart are defined THERE, next to
# instance-up/instance-down, because they are the plane-only half of that pair. They are
# deliberately not repeated here: two recipes for one target is exactly the drift this
# branch removed from the shell side, and `make` can only be right about one of them.

# --- Full rebuild (one command) --------------------------------------------
# A single command that runs everything needed before/for an instance rebuild:
#   1. binaries                 (make build — bin/orchicon + bin/orch are built
#                                FIRST so every check/test exercises the code
#                               about to ship, never a stale binary: the
#                                 real-pty smoke gate executes bin/orch)
#   2. all checks/tests         (make ci:  lint gen vet test rls-check)
#   3. migration hash sync      (make migrate-hash — keeps db/migrations/atlas.sum
#                                 in sync so the Atlas CLI path stays happy)
#   4. frontend + binary        (instance-build force-fe=1 — the frontend is
#                                 rebuilt and embedded into the binary via go:embed.
#                                 NO image is built and no container is created)
#   5. stop/restart the instance (instance-down then instance-up; the plane on THIS
#                                 host boots with MigrateOnBoot=true — serve.go:366 —
#                                 which applies any pending embedded migrations, so
#                                 step 3 is the repo hash sync and step 5 surfaces
#                                 the DB migration)
#   6. cache hygiene            (make cache-trim — placed AFTER the gate that grows the cache, so
#                                 the run that just laid down a day of build artifacts also bounds
#                                 them. It is a NO-OP when the cache is under CACHE_CAP_MB, so a
#                                 healthy dev loop pays nothing for it. This is the "regularly"
#                                 half of the fix: cross-compile no longer dumps foreign-platform
#                                 artifacts into the shared cache, and this keeps the rest honest)
#
# The DB migration itself is applied by the PLANE at boot (migrate.Run), so there is
# no separate `make migrate` needed here — running it against the instance's Postgres
# would race the plane that owns that database.
.PHONY: full-rebuild rebuild-dev rebuild-prod rebuild-test
full-rebuild: ## One command: binary build + all checks/tests + migrate-hash + instance restart (usage: make full-rebuild instance=dev|prod|test)
	@test -n "$(instance_given)" || { echo "usage: make full-rebuild instance=dev|prod|test"; exit 1; }
	$(MAKE) build
	$(MAKE) ci
	$(MAKE) migrate-hash
	$(MAKE) instance-rebuild instance=$(instance)
	$(MAKE) cache-trim

# instance= reaches instance-rebuild EXPLICITLY on the command line: that is how
# the operator's choice survives into the sub-make, whose own guard tests
# instance_given. Nothing here is ever exported globally, so rebuilding one
# instance cannot change another's state.
rebuild-dev: ## One command: full checks/tests + rebuild + restart the DEV instance
	$(MAKE) full-rebuild instance=dev
	$(MAKE) orch-launcher-dev

rebuild-prod: ## One command: full checks/tests + rebuild + restart the PROD instance
	$(MAKE) full-rebuild instance=prod
	$(MAKE) orch-launcher-prod

# rebuild-test mirrors the other two, minus the launcher: a throwaway instance
# exists to be rebuilt and torn down repeatedly, and it needs no dedicated TUI
# client (that is why there is deliberately no `make orch-launcher-test` below —
# a throwaway is reached by running it, not by holding a TUI client pinned to it).
# Teardown is `scripts/orchicon.sh uninstall test --yes`: `make instance-down`
# merely stops it, this REMOVES the instance and its data — which is the whole
# point of a throwaway instance.
rebuild-test: ## One command: full checks/tests + rebuild + restart the TEST instance
	$(MAKE) full-rebuild instance=test

# --- Dual orch launchers ----------------------------------------------------
# Two orch clients on PATH: `orch` tracks bin/orch (dev vintage) and
# `orch-prod` is a snapshot COPY of bin/orch refreshed only by the prod
# path — so a prod-vintage client survives later `rebuild-dev` runs (the
# operator holds prod back when dev has breaking changes).
#
# THERE IS ONE WRAPPER, `orch-dev`, and only one. It presets ORCHICON_URL for
# the dev instance and carries a clearly marked ORCHICON_TOKEN line for the user
# to fill (dev and prod have separate identity stores, so one config token
# cannot serve both). There cannot be an `orch-prod` WRAPPER: that path is
# occupied by the snapshot COPY of the binary above, so a generate-if-absent
# wrapper for it could never be written -- the copy always creates the file
# first, so the block that used to sit in orch-launcher-prod reported "already
# exists" and never ran once. Prod is selected through `orch-prod` itself plus
# the ~/.orchicon/config profile that names the instance. The dev wrapper is
# generated only if absent (never overwritten) and chmod 600.
#
# NO `test` LAUNCHER, DELIBERATELY. The dev/prod split exists because an operator
# holds two LONG-LIVED instances at different vintages; the `test` instance is a
# throwaway that exists to be rebuilt and torn down (see rebuild-test above), so
# it needs no dedicated TUI client and gets no wrapper here. It is still a first
# class instance everywhere else: it has its own port column, data dir and
# database in scripts/lib/instances.sh, and `make rebuild-test` drives it.
LAUNCHER_DIR ?= $(HOME)/.local/bin
.PHONY: orch-launchers orch-launcher-dev orch-launcher-prod
orch-launchers: ## Install/refresh the dual orch launchers into ~/.local/bin (orch + orch-prod + the orch-dev wrapper)
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
	# NO `orch-prod` WRAPPER, AND THAT IS NOT AN OVERSIGHT: `orch-prod` is the
	# SNAPSHOT COPY of the binary made above, so a generate-if-absent wrapper for
	# the same path could never be written -- the copy always creates the file
	# first, so the block reported "already exists" and never ran once. The
	# dev/prod split is carried by the two BINARIES plus the ~/.orchicon/config
	# profile that names the instance; only the dev wrapper needs a token file.
	@echo ""
	@echo "  Tokens are per-instance (dev and prod have separate identity stores)."
	@echo "  Fill ORCHICON_TOKEN in $(LAUNCHER_DIR)/orch-dev; orch-prod selects the"
	@echo "  prod instance through its profile in ~/.orchicon/config."


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

# orch-launcher-prod refreshes the prod snapshot (the orch-prod copy) after a
# prod rebuild. The dev launcher is left untouched, so a prod-vintage
# client survives later rebuild-dev runs.
orch-launcher-prod: ## Refresh the prod orch launcher snapshot after a prod rebuild
	@test -f $(BIN_DIR)/orch || { echo "ERROR: $(BIN_DIR)/orch not found — run 'make build' first"; exit 1; }
	@mkdir -p $(LAUNCHER_DIR)
	@cp -f "$(BIN_DIR)/orch" "$(LAUNCHER_DIR)/orch-prod"
	@chmod +x "$(LAUNCHER_DIR)/orch-prod"
	@echo "==> orch-prod launcher refreshed: $(LAUNCHER_DIR)/orch-prod (snapshot copy, prod vintage)"
	# No wrapper is generated here on purpose -- see the section header: the
	# snapshot copy above IS $(LAUNCHER_DIR)/orch-prod, so a generate-if-absent
	# wrapper for the same path could never be written.

# --- Install ---------------------------------------------------------------
#
# BOTH PASS --instance, because the installer now ASKS which instance and a make target that blocks on a
# question is a make target you cannot run in a script. `install-uninstall` is the destructive one, so its
# instance is stated explicitly rather than left to a prompt or to whatever ORCHICON_INSTANCE is exported
# in the caller's shell — a reflexive Enter on that verb must never choose a stack to destroy.
.PHONY: install-dry-run install-uninstall
install-dry-run: ## Dry-run the install script for the dev instance (no changes made)
	scripts/install.sh --dry-run --instance dev

install-uninstall: ## Uninstall the dev instance via the install script (data preserved; pass instance=test|prod to change)
	scripts/install.sh --uninstall --instance $(if $(instance),$(instance),dev)

# --- CI --------------------------------------------------------------------
# The CI gate, split the same way .github/workflows/ci.yml splits it:
# ci-go is the Go control-plane gate (no full Node install — `gen` pulls
# only the two protoc plugin packages); fe-lint/fe-test are the frontend
# gate and run in the fe CI job. `ci` is the local convenience union.
.PHONY: ci ci-go
# CROSS-PLATFORM COMPILE GATE. The platforms and flags are EXACTLY the release matrix's
# (release.yml: linux/darwin on amd64+arm64, CGO_ENABLED=0, the two shipped binaries), because
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
# Output goes to a temp directory: this is a COMPILE check, not a build, and dropping four pairs of
# binaries into the repo root would leave them behind.
#
# THE CACHE GOES THE SAME WAY AS THE BINARIES — and its absence is why the dev cache reached 71 GB.
# This gate compiles FOUR platforms, and each platform's package archives are ~130 MB, so ONE
# `make ci-go` (and therefore one `make rebuild-dev`) wrote TENS OF GB into the shared dev GOCACHE.
# Those artifacts are worthless to the dev loop — nothing on an amd64 dev host ever links a
# windows/arm64 archive — but they are not free: they are what filled the disk.
#
# Go's own trim could not reclaim them either, because its policy is age-based with a five-day floor
# (trimLimit in cmd/go/internal/cache/cache.go) and a day's cross-compile output is all younger than
# that. The growth was therefore unbounded AND unreclaimable. Isolating it here fixes the growth AT
# THE SOURCE; `make cache-trim` bounds whatever is left.
#
# The cost is measured, not assumed: a COLD cross-compile of the two shipped binaries for one
  # platform takes ~21s and writes ~1.1 GB, so all four from a clean cache is a couple of minutes of
  # CPU — paid once per gate run, in exchange for a shared cache that no longer carries foreign bytes.
  #
  # WINDOWS IS HERE, and it is the point of the difference between this list and the release matrix.
  # This list is the COMPILE GATE, not the ship list: Postgres now runs as a HOST process on Windows
  # (cmd/orchicon/privilege_windows.go, hostproc_windows.go), so a windows/amd64 build break is a
  # release-blocking failure for that host even though no Windows artifact is published. It is not a
  # hypothetical: cmd/orchicon/install_host.go called syscall.Kill from an UNTAGGED file, and
  # windows/amd64 and windows/arm64 failed to build for it while this gate stayed green — because
  # Windows was not in this list. release.yml's matrix is a SEPARATE decision (what SHIPS) and is
  # deliberately left alone; the gate and the matrix now differ on purpose.
  CROSS_PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

# CROSS_TMP is where cross-compile keeps its throwaway cache and output. It is under the repo rather
# than mktemp's default because the default is /tmp, and /tmp is a 2 GB tmpfs on some hosts: this gate
# writes ~1.1 GB of cache per platform, so with Windows included it would fill that tmpfs PART WAY
# THROUGH and surface the disk as a COMPILE ERROR on whichever platform happened to hit it last. The
# directory is gitignored and removed by the trap, so nothing is left behind.
CROSS_TMP := $(CURDIR)/.cross-compile
.PHONY: cross-compile
cross-compile: ## Compile the shipped binaries for every release platform (catches platform-specific breaks)
	@test -n "$(PKGS)" || { if printf '%s\n' "$(GO_LIST_ERR)" | grep -q .; then printf 'ERROR: `go list ./...` FAILED — the local gate cannot run. This is NOT the empty-match case: tmp/ exclusion is not the cause, and the packages were never the problem.\n\n%s\n\nFix the cause above (a module cache or foreign module left in the repo scratch dirs is the usual one), then re-run.\n' "$(GO_LIST_ERR)" >&2; else printf 'ERROR: no packages matched ./... (tmp/ excluded) — refusing to run the gate over nothing\n' >&2; fi; exit 1; }
	@set -e; mkdir -p $(CROSS_TMP); xcache="$$(mktemp -d $(CROSS_TMP)/cache.XXXXXX)"; tmp="$$(mktemp -d $(CROSS_TMP)/out.XXXXXX)"; \
	trap 'rm -rf "$$tmp" "$$xcache"' EXIT; \
	echo "==> cross-compile cache: $$xcache (throwaway — never pollutes $(GOCACHE))"; \
	for t in $(CROSS_PLATFORMS); do \
	  os="$${t%%/*}"; arch="$${t##*/}"; \
	  echo "==> $$os/$$arch"; \
	  GOCACHE="$$xcache" GOOS="$$os" GOARCH="$$arch" CGO_ENABLED=0 $(GO) build -o "$$tmp/" ./cmd/orchicon ./cmd/orch; \
	  GOCACHE="$$xcache" GOOS="$$os" GOARCH="$$arch" CGO_ENABLED=0 $(GO) vet $(PKGS); \
	done; \
	echo "==> all $(words $(CROSS_PLATFORMS)) release platforms compile and typecheck"

# WHY `go vet` IS IN THE LOOP ABOVE, AND NOT ONLY `go build`: `go build` never compiles `_test.go`, so
# a cross-platform break that lives in a TEST file is invisible to it. Not hypothetical: with Windows
# in CROSS_PLATFORMS but only `go build` running, this gate stayed GREEN while
#   GOOS=windows go test -c ./cmd/orchicon
# failed with `cmd/orchicon/binaries_test.go:48:31: undefined: shellQuote` — shellQuote is pure Go,
# but it sat in `//go:build !windows` killorphans_test.go, and binaries_test.go is untagged on purpose
# (it drives the resolver through a `goos` argument, so it builds Windows-shaped paths on every host).
# `go vet` typechecks the package TOGETHER WITH its test files under the target GOOS, so it catches
# exactly that class for ~0.1 s per platform out of a ~21 s cold build. It compiles the tests without
# running them, which is what this gate is: a COMPILE check.
#
# `./...` AND NOT `./cmd/orchicon ./cmd/orch`, and that scope was the second half of the same bug.
# The FIRST fix (1efd8b40) added this vet but pointed it only at the two command packages, so it
# still could not see the rest of the module — and the rest of the module carried the identical
# defect: internal/mcpclient/manager_test.go was untagged, and its orphan/sweep tests call
# `syscall.Kill(pid, 0)` and read `/proc/<pid>/{exe,stat,environ}`:
#
#   GOOS=windows go test -c ./internal/mcpclient
#   internal/mcpclient/manager_test.go:363:20: undefined: syscall.Kill
#
# A gate scoped to the packages a platform happens to break IN is not a gate for that platform.
# `./...` costs ~4 s on top of the two-package vet COLD (measured: 58 s -> 62 s for one platform,
# because the `go build ./cmd/...` above already populates most of the module's cache) — nothing
# beside the ~21 s build it rides on, and it is the difference between "the command packages
# compile" and "the module compiles, tests included, for every platform we gate."
#
# AND IT VETS `$(PKGS)`, WHICH IS THAT SAME `./...` LIST MINUS THE GITIGNORED SCRATCH — not a
# narrowing of the scope argued above. `PKGS` IS a module-wide `go list ./...` (strict, so a real
# module breakage still fails it); all it removes is the packages under tmp/, which have no go.mod
# and are not this module's code. The scope that mattered here — every package of the module,
# test files included, per platform — is intact. What is gone is the ability of a leftover
# scratch file to break the COMPILE gate, which is the one gate that must never be hostage to
# scratch: it is the reason `make vet`/`make test` stayed green while this target failed, and
# the reason a healthy tree could not be rebuilt. Checked, not assumed: `GOOS=windows go list
# ./...` and the host listing return the SAME package set, so resolving the list on the host
# loses nothing platform-specific. The guard above the loop refuses an empty list, because
# `go vet` with no arguments would silently vet the CURRENT DIRECTORY and report success.

# ALL THREE test targets, because each side added one to the same line: this branch adds the
# two script gates (cache-trim-test, disk-report-test) and develop added install-ps1-test. They
# are independent, so the union is the correct resolution — dropping either side would silently
# retire a gate that other work depends on.
ci-go: lint gen-check vet test synth-data rls-check adapter-bake-guard install-ps1-test install-path-test host-residency-test local-services-test orchicon-cli-test no-engine-e2e-assertions-test runtime-e2e-assertions-test cache-trim-test disk-report-test version-tag-match cross-compile ## Run the Go control-plane CI gate (mirrors the go-ci workflow job)
ci: ci-go fe-lint fe-typecheck fe-test site-check version-tag-match ## Run the full CI gate locally (Go + frontend + landing page)

.PHONY: tui-pty-gate
tui-pty-gate: ## Standing real-pty TUI verification gate (smoke + mouse + /connect)
	ORCH_PTY_SMOKE=1 go test ./internal/tui/ -run 'TestPTY' -count=1 -timeout 420s -v
