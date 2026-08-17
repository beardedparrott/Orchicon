# AGENTS.developer.md — Orchicon Development Guide

Read on demand by agents working directly with the human developer (and by human contributors). Orchicon workers read `AGENTS.worker.md` instead — their instructions live in their system prompt.

- **Repo**: https://github.com/beardedparrott/Orchicon.git
- **Language**: Go (control plane) + TypeScript (frontend)
- **Design docs**: `DOCUMENTATION.md` — the single comprehensive documentation file. Read its relevant sections before touching an unfamiliar subsystem, and keep it in sync when you change a feature or architectural component.
- **Everything ships as one binary** (frontend dist, container configs, migrations embedded via `go:embed` in `assets.go`). No branch/commit/PR needed during active development — just build and test.

## Two contexts, two approval flows

A **worker running inside Orchicon** operates autonomously: it PRs into `develop`, merges on approval, and deletes the branch without asking. A **session working directly with a human** must ALWAYS ask the human before creating a PR and again before merging — the human owns the review. When in doubt, treat yourself as a human-facing session and ask.

## Working norms

- **Do not assume; test every hypothesis.** When a user reports a bug, reproduce it, read the relevant code path, and verify the fix resolves it at the system level — not just in a typecheck or unit test.
- **Efficiency is not an excuse for shortcuts.** Never patch a symptom without first identifying the root cause; verbose investigation is cheaper than a second pass that has to undo incorrect assumptions.
- **Every answer must explain the full picture:** *why* it was broken, *where* the fault lived, *who* caused it (which component), *when* it triggers, and *what* the fix does at a mechanical level.
- **Fix the whole class, not just one instance.** A bug in a query, handler, or component — especially a copy-paste pattern like a SQL column list or a Scan call — is often systemic; search for every other occurrence and fix them all.
- **Think broadly, not minimally.** If a suggestion applies to one endpoint (e.g. "add cache-control headers"), check whether every similar path has the same gap.
- **Prefer parallel tool calls** when independent (one message, many tools). Read only the slice of a file you need; keep edits surgical; skip preamble/postamble — the diff speaks for itself. Run `make ci` once at the end, not after every edit.

## Git workflow

- `develop` is the integration branch; `main` is release-only and managed by the human. Workers and agents branch off `develop` and PR into `develop` — never `main`.
- ALWAYS create a new branch before starting work. Never commit to `main` or `develop` (a local pre-commit hook rejects direct commits there; re-create it if missing).
- Branch naming: `<type>/<short-description>` (`feat/`, `fix/`, `chore/`, `refactor/`, `docs/`, `test/`).
- Before starting work on a new branch, bump the version tag (`git tag -a v0.1.<next> -m "v0.1.<next>"`) so local builds report the current version. `git fetch --tags` before rebuilding after a release merge.
- PRs must target `develop` explicitly (`gh pr create --base develop`) — `main` is the default branch. PRs must NOT carry the `release` label (that label belongs only on the human's `develop` → `main` release PR).
- Commit early and often on your branch. Write clear present-tense messages (`Add project CRUD service and data-access layer`). Stage only the files relevant to the commit.
- Before starting work, `git pull origin develop` to get the latest. Before pushing, `git fetch origin && git rebase origin/develop` if the branch has been open for a while.
- Before opening a PR: fetch + rebase onto `origin/develop` if the branch is stale. Update `UPDATES.md` with a new row (typed table format, monotonic row numbers). Do NOT touch README.md's "Last Release Changes" — it is auto-synced from UPDATES.md by `scripts/gen-release-notes.sh --sync-readme`.
- Before merging, ask the user again (PR-creation approval ≠ merge approval). After merging, delete the branch.

## Local development loop

```bash
make container-rebuild instance=dev   # stop dev -> build binary+image -> start dev
make container-build                  # build bin/orchicon + the container image
scripts/container.sh down dev && scripts/container.sh up dev   # restart with the new image
```

Dev and prod are two container instances (`orchicon-cnt-dev` on :8080/:3002, `orchicon-cnt-prod` on :8091/:3003) sharing compose-era Postgres volumes. **The DEV instance is the default and only instance for development work. Never create, mutate, or delete data on the PROD instance.**

**Disk hygiene:** the Go build cache grows to tens of GB (`~/.cache/go-build`) and repeated container builds leave dangling Docker images. Reclaim with `make clean` (go cache + `bin/`) and `make clean-docker` (dangling images + stopped containers + unused volumes). The serve/detached log auto-rotates and is pruned via Settings → Defaults → Log management — never `rm` a live log by hand. Run `make clean` at the end of a heavy dev session, and check `make cache-check` before starting work.

## Phases

Every task follows this sequence:

1. Read AGENTS.md, then read UPDATES.md to understand the current state, and read the relevant DOCUMENTATION.md sections before touching something unfamiliar.
2. Create a branch and do the work, committing changes often.
3. Fully test and verify (see Verification).
4. Before the final commit on your branch, update `UPDATES.md` with a new row in the typed table format (see UPDATES.md). Do NOT touch README.md's "Last Release Changes" manually — `scripts/gen-release-notes.sh --sync-readme` keeps it in sync. This is the commit that will be PR'd and merged into `develop`.
5. Follow the Git workflow above.
6. Inform the user every time UPDATES have been made, in a tabled format.

If architecture or anything referenced in the docs has changed, update the relevant `.md` documentation for future runs. Do not edit AGENTS.md itself — it is the human-maintained router; flag any proposed change to the human.

## Architecture quick reference

- Control plane: Go, single binary, k8s-style reconcilers. API: Protobuf + Connect (gRPC + REST + streaming from one schema). DB: PostgreSQL 16 with RLS + transactional outbox. Event bus: NATS JetStream. Telemetry: OpenTelemetry → Grafana stack (Tempo + Loki + VictoriaMetrics). Deployment: single container (`orchicon container` PID-1 supervisor, `scripts/container.sh` manages dev/prod instances).
- Runtime adapters: gRPC sidecars (OpenCode first). Adapters are mounted, never baked — the operator installs opencode on the host; `container.sh`/`orchicon install`/the runtime daemon bind-mount `~/.opencode` into the main and runtime containers. Session transport is the adapter contract: worker executions run as persistent opencode sessions (`internal/opencode/session.go`), not one-shot `run` subprocesses.
- Frontend: TypeScript + React + Vite + Connect-ES + React Flow.
- Workflow runtime containers: pure per-workflow execution on a warm pool, leased exclusively per run, reset to pristine on release. `:orchicon-dev` images boot a disposable in-container sandbox plane (Postgres → NATS → `orchicon serve` at `http://localhost:8080`) so workers get a consistent environment and the `orchicon_*` MCP tools against the sandbox DB — never the host plane's.
- Recovery follows a default 6-step workflow (capture → summarize → preserve → review → plan → resume) with bounded auto-relax and L1→L2→L3 escalation. Policy engine uses OPA (Rego) with bundles from Postgres. Auth is OIDC with API keys (SHA-256 hashed) + RBAC. Webhooks via NATS consumer with HMAC signing and a replayable dead-letter queue.

## Key invariants (do not violate)

1. No business logic in the frontend — the UI reflects server state.
2. No hand-written API URLs — use the generated Connect-ES client.
3. No mutations outside the transactional outbox pattern.
4. No raw SQL outside the data-access layer.
5. Every `tenant_id` table must have an RLS policy (CI gate enforces).
6. Adapters never touch Postgres or NATS directly — gRPC stream only.
7. No automatic model failover — the human defines the exact model.
8. Recovery is opt-out, not opt-in.
9. Migrations are forward-only.
10. Windows is always considered — delivered by running the whole Linux stack inside WSL2, no native Windows port.

## Security standards (floor, not ceiling)

- **Secrets**: no secrets in code/commits/logs; env vars or a secret store only. API keys hashed at rest; passwords never stored (OIDC). Dev-only credentials are placeholders.
- **Input validation**: validate at the API boundary (see `internal/project/validate.go` for the pattern). Parameterized queries only. JSON fields validated as JSON before storage. Size bounds on all inputs. Slugs regex-validated (`^[a-z0-9]+(?:-[a-z0-9]+)*$`); IDs are server-generated ULIDs.
- **Tenant isolation**: every request tenant-scoped; RLS is the backstop. The data-access layer injects `tenant_id` into every WHERE/INSERT.
- **Frontend**: browser never stores long-lived secrets (access tokens in memory, refresh tokens in HttpOnly cookies). Client-side validation is UX, not the security gate.

## Tooling hints

- Use `context7` for library docs and `gh_grep` for real GitHub usage examples.
- LSP servers (gopls, typescript, eslint) are enabled; `make ci` is the authoritative gate.
- Playwright MCP is configured in `opencode.jsonc` (Chrome installed). NEVER use Firefox for verification.
- The Orchicon MCP is registered in `opencode.jsonc` as the `orchicon` server against `orchicon-cnt-dev` (tenant `tnt_dev`). `orchicon-prod` is registered but disabled.
- NEVER run a foreground server from a shell tool — it never returns. Use `orchicon serve --detach` (stop with `--stop`, logs in `.dev/logs/orchicon.log`) or `scripts/container.sh` (starts detached). Never `pkill -f` a pattern that could match your own shell command; kill by PID or port (`fuser -k 8080/tcp`).
- UI consistency: every list page follows the same pattern — search input, filter/sort dropdowns, select-all checkbox, per-item checkboxes, selection count, bulk action button.

## Verification

Compilation passing is not working. Verify runtime behavior. At minimum: `make ci` passes end-to-end; the container instance starts healthy; migrations apply cleanly on a fresh data volume; the control plane boots and serves (`curl localhost:8080/healthz`); the frontend renders; runtime calls use the REAL opencode runtime with a free model (`opencode/deepseek-v4-flash-free`), never simulation mode (`ORCHICON_SIMULATE_ADAPTER=1` is offline dev only). Stall/wall-clock guardrails default: no-progress 300s, no-file-diff 15m, repetition 5×/300s, wall-clock 3600s (absent → defaults to 3600).

For Docker/infra changes, verify the full stack boots (healthz + Grafana on :3002 + telemetry flows), data preservation across compose-era volumes, and the fresh-boot path. **Do not claim "done" without having run the thing.** The canonical test is a release binary + container image run like a user would.

## Dev control script

`scripts/container.sh` manages the single-container instances: `build`, `up dev|prod`, `rebuild dev|prod`, `down dev|prod`, `status`, `logs`, `ps` (or `make container-*`). Data volumes live in Docker named volumes plus the compose-era Postgres volumes.

## Install scripts & release

`scripts/install.sh` / `scripts/install.ps1` are the one-line installers published at orchicon.dev. Windows runs the whole stack inside WSL2 (`install.ps1` provisions WSL2 + Docker-in-WSL and installs the Linux binary into the distro). `site/` + install scripts are staged on `develop` and only reach orchicon.dev when the human merges `develop` → `main` (CloudFlare Pages is pinned to `main`). The release workflow builds binaries for linux/darwin/windows × amd64/arm64 on tag push, attaches them to the GitHub Release, and pushes the container images. Releases are capped at 5 (`prune-releases.yml`); the release body is generated from UPDATES.md by `scripts/gen-release-notes.sh`. When a phase changes what ships in the binary, update the install scripts and release workflow. Verify by running the installer against a draft release at minimum (`bash scripts/install.sh --version vX.Y.Z --dry-run` on each target platform, or `--uninstall` to test cleanup).

## E2E & data preservation

- Back up the database before any agent session that modifies data: `docker exec orchicon-postgres pg_dump -U orchicon -d orchicon > /tmp/orchicon-backup-<ts>.sql`.
- Every `REFERENCES` column must carry `ON DELETE SET NULL` or `ON DELETE CASCADE`.
- Every `ALTER TABLE ADD COLUMN` must use `ADD COLUMN IF NOT EXISTS`.
- Seed data is managed in Go (`internal/db/seed_workers.go`), not SQL migrations.

## Ask Orchicon — keep it in sync

Every time you add/change/remove a first-class entity, RPC, or user-facing capability, update the Ask Orchicon agent to match. The tool surface is `internal/askorchicon/tools.go` (`allTools()`), tool implementations in `tool_*.go` (one per domain), the agent identity in `agent.go`, defaults in `service.go`'s `defaultAgentConfigProto()`. The registry is what the Orchicon MCP server exposes (`orchicon mcp`, `internal/mcp/`) — `BuildConfigContent` registers it by default in every opencode run.

## Things you need to know

- Connect-ES codegen is pinned to local v1 npm plugins. Atlas RLS policies are hand-appended SQL — after hand-editing a migration run `make migrate-hash`.
- `orchicon container` runs the whole stack as PID-1. `orchicon serve` runs the plane headless. Reconcilers use `pg_try_advisory_lock` for per-kind leadership. NATS subscribers fan events out to streaming RPCs.
- Worker lifecycle: draft → published → deprecated → retired (published versions immutable). WorkItem hierarchy: Epic → Feature → Task → Subtask (max 4 levels). Dependency edges form a DAG with cycle detection.
- TaskReconciler is the only component that creates WorkerExecutions. A work item bound to a workflow run goes `running` at run start and `succeeded`/`failed` at run end — the step run is the execution + recovery unit; the composite prompt lives on the step run (`_prompt`).
- Policies use OPA v1 with bundles from Postgres, narrowest-scope-first, fail-open default. BlobStore has local-filesystem and S3 backends. Markdown is supported on all prompt-affecting fields.

## UPDATES.md

Read it before starting any work. All changes are recorded there in the typed table format (`| # | Type | Phase | One-line summary |`), rows appended to the top with monotonic row numbers — never renumber. Type ∈ `Feature | Bug fix | Chore | Docs | Refactor | Test`. Release-notes automation derives the release body from UPDATES.md; README's "Last Release Changes" stays in sync via `gen-release-notes.sh --sync-readme` (on develop, staged; on release, published; after a release, run `--trim`).