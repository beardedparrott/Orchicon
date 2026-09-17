# developer.md — Orchicon Development Guide

Read on demand by agents working directly with the human developer (and by human contributors). Orchicon workers read `worker.md` instead — their instructions live in their system prompt (and `worker.md` is injected into their session).

- **Repo**: https://github.com/beardedparrott/Orchicon.git
- **Language**: Go (control plane) + TypeScript (frontend)
- **Design docs**: `DOCUMENTATION.md` — the single comprehensive documentation file. Read its relevant sections before touching an unfamiliar subsystem, and keep it in sync when you change a feature or architectural component.
- **Stale citations in source comments**: comments all over the codebase still cite the old numbered specs — `docs/07_API_Specification.md §3.3`, or the short form `docs/05 §4`. **Those files no longer exist.** Commit `83b38e0f` deleted all eleven (`docs/00`…`docs/10`) and replaced them with `DOCUMENTATION.md`. Around 860 such references remain in hand-written Go/TS and `.proto` (plus their mirrors in generated code). They are DEAD POINTERS: there is nothing to go read.
  - **Do not "fix" one by repointing it at `DOCUMENTATION.md` unless the target content is actually there.** Most of it is not — `DOCUMENTATION.md` has no section on the API specification, the Adapter SDK, or the worker specification, and it is not `§`-numbered. A citation that resolves to the wrong place reads as authoritative, which is worse than one that plainly points nowhere.
  - **The code is the authority** for what these citations describe (lifecycle rules, schema, API behaviour). Read the enforcement point and cite THAT in a comment, e.g. `internal/db/worker.go UpdateWorker is the gate` instead of `docs/05 §4`.
  - A sweep to remove them is deliberately deferred: it touches ~860 sites, and the obvious scripted approach is unsafe — an early attempt broke `internal/guard/guard.go` by rewriting an embedded shell script and deleted `()` from `db.NewID()` by matching a function call, while the test suite stayed green. Treat it as its own reviewed work item, per area.

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
- The version tag (`vX.Y.<n>`) is bumped automatically on every merge to `develop` (`.github/workflows/develop-bump.yml`), so local builds report the current version. `git fetch --tags` before rebuilding to pick up the latest tag — `git pull` does not fetch tags.
- PRs must target `develop` explicitly (`gh pr create --base develop`) — `main` is the default branch. PRs must NOT carry the release labels (`release`, `release-minor`, `release-major` — those belong only on the human's `develop` → `main` release PR: next patch / next minor / next major respectively, computed by `.github/workflows/auto-release.yml`).
- Commit early and often on your branch. Write clear present-tense messages (`Add project CRUD service and data-access layer`). Stage only the files relevant to the commit.
- Before starting work, `git pull origin develop` to get the latest. Before pushing, `git fetch origin && git rebase origin/develop` if the branch has been open for a while.
- Before opening a PR: fetch + rebase onto `origin/develop` if the branch is stale. Update `UPDATES.md` with a new row (typed table format, monotonic row numbers). Do NOT touch README.md's "Last Release Changes" — it is auto-synced from RELEASE_HIGHLIGHTS.md (compact digest) by `scripts/gen-release-notes.sh --sync-readme`.
- Before merging, ask the user again (PR-creation approval ≠ merge approval). After merging, delete the branch.

## Local development loop

```bash
make container-rebuild instance=dev   # stop dev -> build binary+image -> start dev
make container-build                  # build bin/orchicon + the container image
scripts/container.sh down dev && scripts/container.sh up dev   # restart with the new image
```

Dev and prod are two container instances (`orchicon-cnt-dev` on :8080/:3002, `orchicon-cnt-prod` on :8091/:3003) sharing compose-era Postgres volumes. Dev is the default instance for hands-on development; prod runs the pipeline (the work items, workers, and workflows you schedule there). Worker access to an instance's data is role-scoped through the worker's identity, and the `:orchicon-dev` sandbox plane keeps worker DB tests out of real data — there is no dev-vs-prod instance choice a worker must manage.

**Disk hygiene:** the Go build cache grows to tens of GB (`~/.cache/go-build`) and repeated container builds leave dangling Docker images. Reclaim with `make clean` (go cache + `bin/`) and `make clean-docker` (dangling images + stopped containers + unused volumes). The serve/detached log auto-rotates and is pruned via Settings → Defaults → Log management — never `rm` a live log by hand. Run `make clean` at the end of a heavy dev session, and check `make cache-check` before starting work.

## Phases

Every task follows this sequence:

1. Read AGENTS.md, then read UPDATES.md to understand the current state, and read the relevant DOCUMENTATION.md sections before touching something unfamiliar.
2. Create a branch and do the work, committing changes often.
3. Fully test and verify (see Verification).
4. Before the final commit on your branch, update `UPDATES.md` with a new row in the typed table format (see UPDATES.md). Do NOT touch README.md's "Last Release Changes" manually — `scripts/gen-release-notes.sh --sync-readme` keeps it in sync with RELEASE_HIGHLIGHTS.md. This is the commit that will be PR'd and merged into `develop`.
5. Follow the Git workflow above.
6. Inform the user every time UPDATES have been made, in a tabled format.

If architecture or anything referenced in the docs has changed, update the relevant `.md` documentation for future runs. Do not edit AGENTS.md itself — it is the human-maintained router; flag any proposed change to the human.

## Architecture quick reference

- Control plane: Go, single binary, k8s-style reconcilers. API: Protobuf + Connect (gRPC + REST + streaming from one schema). DB: PostgreSQL 16 with RLS + transactional outbox. Event bus: NATS JetStream. Telemetry: OpenTelemetry → Grafana stack (Tempo + Loki + VictoriaMetrics). Deployment: single container (`orchicon container` PID-1 supervisor, `scripts/container.sh` manages dev/prod instances).
- Runtime adapters: gRPC sidecars (OpenCode first). Adapters are mounted, never baked — the operator installs opencode on the host; `container.sh`/`orchicon install`/the runtime daemon bind-mount `~/.opencode` into the main and runtime containers. Session transport is the adapter contract: worker executions run as persistent opencode sessions (`internal/opencode/session.go`), not one-shot `run` subprocesses.
- Frontend: TypeScript + React + Vite + Connect-ES + React Flow.
- Workflow runtime containers: pure per-workflow execution on a warm pool, leased exclusively per run, reset to pristine on release. `:orchicon-dev` images boot a disposable in-container sandbox plane (Postgres → NATS → `orchicon serve` at `http://localhost:8080`) so workers get a consistent environment and the sandbox `orchicon_*` MCP tools against the sandbox DB — never the host plane's. Separately, the plane-channel MCP (`orchicon_plane_*`) is registered on **every** runtime image for role-bound workers — plane access is role-gated, never image-gated.
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
11. **Engine behaviour is never keyed on a tenant's identifiers** — no step id, project id, title, or worker id in a reconciler branch. Recorded policy is config; anything else is DERIVED from structure (the graph, the kind, the run's own rows). Two live loop decisions were once rescued and one stranded by a guard comparing `loop_branch == "step-devops-pr"` and a hardcoded success-branch list, which broke silently for every workflow that named its end step differently.

## Security standards (floor, not ceiling)

- **Secrets**: no secrets in code/commits/logs; env vars or a secret store only. API keys hashed at rest; human passwords are stored only by the embedded identity provider (argon2id PHC strings, bcrypt accepted on verify via prefix dispatch, `internal/auth/op`), never by control-plane business logic; external auth flows through OIDC. Dev-only credentials are placeholders.
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

`scripts/install.sh` / `scripts/install.ps1` are the one-line installers published at orchicon.dev. Windows runs the whole stack inside WSL2 (`install.ps1` provisions WSL2 + Docker-in-WSL and installs the Linux binary into the distro). `site/` + install scripts are staged on `develop` and only reach orchicon.dev when the human merges `develop` → `main` (CloudFlare Pages is pinned to `main`). The release workflow builds binaries for linux/darwin/windows × amd64/arm64 on tag push, attaches them to the GitHub Release, and pushes the container images. Releases are capped at 5 (`prune-releases.yml`); the release body is the curated narrative from RELEASE_HIGHLIGHTS.md rendered by `scripts/gen-release-notes.sh` (README's "Last Release Changes" gets the compact digest via `--sync-readme`); the UPDATES.md rows are the internal engineering log. Release versioning: the develop→main PR's label picks the bump — `release` (next patch), `release-minor` (next minor), `release-major` (next major) — computed by `auto-release.yml` from the highest existing tag. When a phase changes what ships in the binary, update the install scripts and release workflow. Verify by running the installer against a draft release at minimum (`bash scripts/install.sh --version vX.Y.Z --dry-run` on each target platform, or `--uninstall` to test cleanup).

## E2E & data preservation

- Back up the database before any agent session that modifies data: `docker exec orchicon-cnt-dev pg_dump -U orchicon -d orchicon > /tmp/orchicon-backup-<ts>.sql`. (Postgres runs INSIDE the instance container — `orchicon-cnt-dev` / `orchicon-cnt-prod` — there is no separate `orchicon-postgres` container.)
- Every `REFERENCES` column must carry `ON DELETE SET NULL` or `ON DELETE CASCADE`.
- Every `ALTER TABLE ADD COLUMN` must use `ADD COLUMN IF NOT EXISTS`.
- Seed data is managed in Go (`internal/db/seed_workers.go`), not SQL migrations.

## Ask Orchicon — keep it in sync

Every time you add/change/remove a first-class entity, RPC, or user-facing capability, update the Ask Orchicon agent to match. The tool surface is `internal/askorchicon/tools.go` (`allTools()`), tool implementations in `tool_*.go` (one per domain), the agent identity in `agent.go`, defaults in `service.go`'s `defaultAgentConfigProto()`. The registry is what the Orchicon MCP server exposes (`orchicon mcp`, `internal/mcp/`) — `BuildConfigContent` registers it by default in every opencode run.

## Both clients — GUI and TUI — stay in lockstep

Orchicon ships **two first-class clients over the same API**: the GUI (`frontend/src/**`) and the TUI (`internal/tui/**`, `cmd/orch`). Any work in Orchicon must consider BOTH on changes that need made.

- Touching a field, entity, action, setting, RPC, or capability means checking both clients' exposure of it. Implement it in both, or exclude one DELIBERATELY with the reason in a code comment and in the change description.
- A capability in one client and not the other is incomplete work, not a follow-up. Parity gaps are the reason this rule exists: cheap to close with the context loaded, expensive to reconstruct later.
- The historical drift is real and instructive: the GUI carried the budget-warning message copy and the loop-decision policy while the TUI exposed neither, and the TUI's workflow step editor still asked operators to type a raw JSON config. Neither client was wrong on its own; the asymmetry was the bug.
- Prefer shared truth over duplicated logic. Where both clients must agree (a vocabulary, a default, a validation rule), keep the source in the API/DB and let each client read it, rather than re-implementing the rule twice.

## Platform-owned contracts (do not make them configurable)

The **task verdict** is contract, not preference. Every worker ends its output with `ORCHICON WORKER SUMMARY: success` / `failure`; `db.WorkerIdentityPreamble` sends every worker to that contract and the seeded prompts spell it out literally in 18 places; `extractSummaryDecision` → `firstWordAsDecision` (`internal/scheduler/reconciler.go`) NORMALIZES exactly those two words and passes any other first word through verbatim; and that word routes the workflow.

- Because a custom word technically passes through, `loopDecisionConfig`'s `success_value` / `failure_value` / `decision_field` are technically settable — and deliberately **exposed in neither client's step editor**. Pointing a gate at a word no worker emits makes every verdict miss, so every gate falls through to the missing-verdict path and fails at run time. Nothing validates the prompt against the config, so the failure is silent and total. They remain settable in the config for a programmatic workflow; they are not a knob.
- `decision_field` is the same class for a second reason: the primary path decodes the upstream step run's decision from a hardcoded `_decision` tag, and only the legacy ticket fallback honours the config key. Offering it would move a knob that mostly does nothing.
- The general rule: **never ship a knob nothing honours.** Verify a field has a real consumer by finding the code that ACTS on it, not the code that parses it into a struct. See the `retry_delay_seconds` note below.

## Loop and approval gates — how a missing verdict is decided

A `loop_decision` routes on an upstream verdict. When NO upstream supplies one, the behaviour is now recorded policy rather than an engine guess:

- `config.on_missing_decision` (`loopDecisionConfig`, `internal/scheduler/workflow_reconciler.go`) is `reask` (default), `success`, or `fail`. An ABSENT key — and any value outside that vocabulary — resolves to `reask`, which is what makes the policy purely additive: every workflow written before it behaves as it did. `reask` re-dispatches the reviewer up to `config.max_reask` (default 3) and then FAILS the node; `success` proceeds forward; `fail` refuses immediately. Both clients expose it, as they do `max_reask`.
- `success` is the right policy for a gate whose only upstream emits no verdict: the re-ask re-dispatches the SAME step, and the loop target IS that step, so the "re-ask" is a loop carrying no new information. That shape is the terminal devops loop — devops → loop_decision, loop → devops, success → end — which exists so a DevOps worker can run again before the workflow finalizes.
- Backfill: `db/migrations/20260924000000_backfill_loop_decision_missing_verdict.sql` (+ `_down`) sets `success` on the steps that need it, and `internal/db/seed_workflows.go` carries the key so new instances are right from birth. Its predicate is STRUCTURAL (loop_decision, object config, exactly one dependency, that dependency IS the loop target) rather than a list of ids, so it stays correct for renamed steps. **Any change to gate routing needs the same three-part treatment: engine + backfill for existing instances + seed for new ones.**
- The migration runner applies pending migrations at boot BEFORE the control plane constructs its reconcilers (`cmd/orchicon/serve.go`, `MigrateOnBoot` defaults true), which is what makes an engine change safe to pair with a backfill: no boot can read the new code against data the backfill has not yet written.
- RLS note: `workflow_versions` is `ENABLE` + `FORCE ROW LEVEL SECURITY`, so a backfill UPDATE in a migration is subject to it — EXCEPT that the migration role is the bootstrap superuser (`initdb -U orchicon`, `cmd/orchicon/container.go`), and superusers bypass FORCE RLS. This is the same reasoning `20260806000000_normalize_work_item_kind.sql` already relies on.

## Things you need to know

- **Dead knobs**: `recovery_executions.retry_delay_seconds` and the matching `retry_delay_seconds` step-config key were REMOVED from the code — the value was written from a constant and parsed off a task step's config, and NO code ever read it back, because execution dispatch has no deferral mechanism at all (the only `next_attempt_at` machinery in the tree is for webhook deliveries). The COLUMN is retained because up migrations are additive-only (no destructive DDL), and `20260925000000_retire_recovery_retry_delay.sql` corrects its comment to say so; a stored `retry_delay_seconds` in an existing step config is now ignored by the engine and preserved verbatim by both step editors. The same file's stale claim that `ORCHICON_RECOVERY_MAX_RETRIES` / `ORCHICON_RECOVERY_RETRY_DELAY_SECONDS` override the recovery defaults was false — neither name was ever read — and is corrected at source in `internal/recovery/engine.go`.
- `db/migrations/atlas.sum` is NOT verified by anything: `internal/migrate` reads the `*.sql` files directly and `tools/atlas-ci` does not check the sum. It is regenerated by `make migrate-hash` (needs the atlas binary, `make tools`) as part of `make full-rebuild`.
- Connect-ES codegen is pinned to local v1 npm plugins. Atlas RLS policies are hand-appended SQL — after hand-editing a migration run `make migrate-hash`.
- `orchicon container` runs the whole stack as PID-1. `orchicon serve` runs the plane headless. Reconcilers use `pg_try_advisory_lock` for per-kind leadership. NATS subscribers fan events out to streaming RPCs.
- Worker lifecycle: draft → published → deprecated → retired (published versions immutable). WorkItem hierarchy: Epic → Feature → Task → Subtask (max 4 levels). Dependency edges form a DAG with cycle detection.
- TaskReconciler is the only component that creates WorkerExecutions. A work item bound to a workflow run goes `running` at run start and `succeeded`/`failed` at run end — the step run is the execution + recovery unit; the composite prompt lives on the step run (`_prompt`).
- Policies use OPA v1 with bundles from Postgres, narrowest-scope-first, fail-open default. BlobStore has local-filesystem and S3 backends. Markdown is supported on all prompt-affecting fields.
- RBAC: identities bind roles (roles carry `entitlements`, the `admin` role is immutable and bypasses). Roles CRUD + Manage Identity Roles live in Admin. Work items and projects can be archived (terminal + childless) and restored. A git-backed run's real PR link is captured from the worker's `PR_URL:`/`PR_STATE:` summary lines; `orchicon backfill-pr` idempotently backfills URLs onto already-completed runs.

## UPDATES.md

Read it before starting any work. All changes are recorded there in the typed table format (`| # | Type | Phase | One-line summary |`), rows appended to the top with monotonic row numbers — never renumber. Type ∈ `Feature | Bug fix | Chore | Docs | Refactor | Test`. The release body comes from RELEASE_HIGHLIGHTS.md (curated narrative; see scripts/gen-release-notes.sh); README's "Last Release Changes" stays in sync as a compact digest via `gen-release-notes.sh --sync-readme` (on develop, staged; on release, published; after a release, run `--trim` to drop released rows).