# AGENTS.md — Development Guidelines for AI Agents

> This file is the entry point for any AI agent working on Orchicon.
> Read it before making any changes.

## Project

- **Repo**: https://github.com/beardedparrott/Orchicon.git
- **Language**: Go (control plane) + TypeScript (frontend)
- **Design docs**: `docs/` — read the relevant doc before touching a subsystem

## WARNING

> **STOP!** Are you in a custom branch or are you on `main`? If you are on `main`. **DO NOT proceed.**
> Instead, create a new branch and continue. Keep this in your memory context **AT ALL TIMES**.
> This is **RULE number 1**.

## Token discipline

The project's model spend is rising. Be economical but **never at the expense of rigor**:

- **Efficiency is not an excuse for shortcuts.** Never patch around a symptom without first identifying the root cause through proper troubleshooting. Verbose investigation is cheaper than a second pass that has to undo incorrect assumptions.
- **Do not assume.** Test every hypothesis. When a user reports a bug, reproduce it, read the relevant code path, and verify the fix actually resolves the issue at the system level — not just in a typecheck or unit test.
- **Every answer must explain the full picture:** *why* it was broken, *where* the fault lived, *who* caused it (which component), *when* it triggers (startup, every request, only after certain conditions), and *what* the fix does at a mechanical level.
- Prefer parallel tool calls when independent (one message, many tools) to cut round-trips.
- Read only the slice of a file you need; avoid re-reading whole files.
- Keep edits surgical — match surrounding style, don't reflow untouched code.
- Skip preamble/postamble in responses; the diff speaks for itself.
- Run `make ci` once at the end, not after every edit.

## Git Workflow

- ALWAYS create a new branch before starting work. NEVER commit to main.
- Branch naming: `<type>/<short-description>` (e.g. `feat/project-crud`, `fix/outbox-relay-dedup`, `chore/docker-compose-setup`). Types: `feat`, `fix`, `chore`, `refactor`, `docs`, `test`.
- Commit early and often on your branch. Write clear commit messages in present tense: `Add project CRUD service and data-access layer`. Stage only the files relevant to the commit.
- Once work is complete and properly tested, ask the user to verify.
- After the user confirms, create a PR and merge. PRs MUST carry the `release` label to kick off the release creation on GitHub.
- **Before every PR merge**, update the "Last Release Changes" section in `README.md` with the new version tag and a one-paragraph summary of the most recent changes.
- Before starting work, always `git pull origin main` to get the latest. Before pushing, `git fetch origin && git rebase origin/main` if the branch has been open for a while.

## Phases

Every task follows this sequence:

1. Read AGENTS.md first.
2. Read any docs or code necessary to perform the work.
3. Create a branch and do the work, committing changes often.
4. Fully test and verify.
5. Follow the Git Workflow above.
 6. Once the PR is merged, update `UPDATES.md` with what was done in the same table format as existing entries.
 7. Inform the user every time UPDATES have been made. Show them in a tabled format what was changed and updated.

If architecture or anything referenced in AGENTS.md has changed, update this file for future agent runs.

## Architecture Quick Reference

- **Control plane**: Go, single binary, k8s-style reconcilers
- **API**: Protobuf + Connect (gRPC + REST + streaming from one schema)
- **Database**: PostgreSQL 16 with RLS + transactional outbox
- **Event bus**: NATS JetStream
- **Telemetry**: OpenTelemetry → SigNoz (ClickHouse) — separated infra
- **Policy**: Rego (Open Policy Agent)
- **Runtime adapters**: gRPC sidecars (OpenCode first, CLI now / IPC later)
- **Frontend**: TypeScript + React + Vite + Connect-ES + React Flow
- **Object storage**: BlobStore abstraction (S3 + local filesystem)
- **Deployment**: Fully local (no cloud) is a supported mode

## Key Invariants (do not violate)

1. No business logic in the frontend — the UI reflects server state.
2. No hand-written API URLs — use the generated Connect-ES client.
3. No mutations outside the transactional outbox pattern.
4. No raw SQL outside the data-access layer.
5. Every `tenant_id` table must have an RLS policy (CI gate enforces).
6. Adapters never touch Postgres or NATS directly — gRPC stream only.
7. No automatic model failover — the human defines the exact model.
8. Recovery is opt-out, not opt-in.
9. Migrations are forward-only.

## Security Standards (applies to every slice)

Every piece of functionality built in this repo must follow these security standards. They are the floor, not the ceiling — review them when adding any new RPC, handler, or frontend form.

### Secrets & credentials

- **No secrets in code or commits.** DSNs, API keys, tokens, and passwords come from the environment (`internal/config`) or a secret store — never hardcoded, never committed. The `.env.example` file documents the variables without containing real values.
- **No secrets in logs.** Never log DSNs, tokens, passwords, or full request payloads that may carry credentials. The slog setup in `cmd/orchicon/main.go` logs structured fields; only log non-sensitive identifiers (tenant id, project id, trace id).
- **Hashed at rest.** API keys are hashed before storage (never plaintext). Passwords are never stored by the control plane (OIDC handles authentication). See docs/07 §6.1.
- **The dev stack credentials** in `deploy/compose/docker-compose.yml` (e.g. `orchicon:orchicon`) are local-dev-only placeholders. They must never appear in a production deployment config.

### Input validation & sanitization

- **Validate at the API boundary.** Every RPC handler validates and sanitizes input before it reaches the data-access layer. See `internal/project/validate.go` for the pattern: trim, bound-check length, regex-validate structured fields (e.g. slug), and reject malformed data with `connect.CodeInvalidArgument`.
- **Parameterized queries only.** All SQL uses pgx parameterized queries (`$1`, `$2`, …). No string interpolation of user input into SQL, ever. The data-access layer (`internal/db`) is the only place SQL lives (invariant #4).
- **JSON fields are validated.** JSON-typed columns (e.g. `goals`) must be parsed/validated as valid JSON before storage. Reject malformed JSON at the handler, not the database.
- **Size bounds on all inputs.** Every text input has a max length enforced at the handler to prevent memory-exhaustion abuse.
- **Slugs and identifiers are regex-validated.** Slugs match `^[a-z0-9]+(?:-[a-z0-9]+)*$`; IDs are ULIDs generated server-side, never accepted from the client on create.

### Tenant isolation

- **Every request is tenant-scoped.** The middleware resolves the tenant and the data-access layer sets `app.tenant_id` per transaction. RLS is the backstop — even a buggy query cannot leak cross-tenant data (docs/09 §8.5).
- **No cross-tenant queries.** The data-access layer injects `tenant_id` into every `WHERE` and `INSERT`. A query without a tenant scope is a bug, not an optimization.

### Frontend

- **The browser never stores long-lived secrets.** Access tokens live in memory; refresh tokens in HttpOnly secure cookies (docs/10 §7). API keys are for headless/CI clients only.
- **Client-side validation is UX, not security.** Zod schemas in forms improve the user experience but every rule is re-validated server-side. Never trust client-side validation as the security gate.
- **No business logic in the frontend** (invariant #1). The UI does not make policy, scheduling, or recovery decisions.

## Tooling hints

- When you need library docs (Connect-ES, Atlas, TanStack Router, pgx, NATS, SigNoz), use `context7` tools before guessing.
- If unsure how to use a library or pattern, use `gh_grep` to search real GitHub usage examples.
- LSP servers (gopls, typescript, eslint, yaml-ls) are enabled — diagnostics surface in the edit loop. Treat them as fast feedback; `make ci` is the authoritative gate.
- Playwright MCP is configured in `opencode.jsonc` for browser testing. Use Chrome and/or Playwright for frontend verification. NEVER use Firefox — the developer uses Firefox and testing needs a separate browser.
- The `site/` landing page and `README.md` document the `orchicon` commands and installed files. Keep both in sync when commands, flags, or install paths change. The CloudFlare Pages build copies `scripts/install.{sh,ps1}` to the deployed site.

## Verification

> **Compilation passing is not the same as working.**
> Agents must verify runtime behavior, not just `go build` / `tsc`.

Before marking a phase or task as complete, verify the following at minimum (adapt to what the change touches):

1. **`make ci` passes end-to-end** — buf lint, codegen, go vet/test, RLS gate. This is the authoritative CI gate.
2. **Dev stack starts healthy** — `make up` then `make ps` shows all containers `healthy` (not just `running`). When the change touches Docker or infrastructure:
   - Check that ZooKeeper is NOT listed in `make ps` output.
   - Verify all 6 containers (postgres, nats, clickhouse, signoz-schema-migrator, otel-collector, signoz) show up.
   - Run `make nuke` then `make up` from a clean slate to verify the full startup sequence works end-to-end.
3. **Migrations apply cleanly** — `make migrate` against the compose Postgres; `make rls-check` passes.
4. **Control plane boots and serves** — `make build && make run`, then `curl http://localhost:8080/healthz` returns `{"status":"ok"}`. Time this command — if the telemetry stack is starting, the boot should still be <2s (not 20s+). Check the control plane logs for `"otel pipeline initialized"` — if it appears before the 2s mark, the non-blocking OTel dial is working.
5. **Frontend renders** — `make fe-dev` (or `npx vite`), then `curl http://localhost:5173/` returns HTTP 200 with the app shell.
6. **Runtime calls are real, not simulated** — end-to-end verification that exercises adapter dispatch MUST call the real `opencode` runtime with a **free model** (e.g. `opencode/deepseek-v4-flash-free`), never the simulation-mode fallback. Simulation mode is a development aid for the offline case only; it must not be used to "verify" dispatch, recovery, or any flow that depends on adapter telemetry. If `opencode` is absent from PATH, fix the environment (install it) — do not fall back to simulation and claim the slice works. Seed workers / executions used for verification must pin a free model in `model_ref` so verification is reproducible at no cost.
   - **Stall + wall-clock guardrails** (docs/06 §2 triggers): the opencode adapter bridge runs a per-execution progress monitor that detects stuck-looping and triggers recovery (opt-out, idempotent). Three stall signals, configurable via env: `ORCHICON_STALL_NO_PROGRESS_WINDOW` (default 120s — no step_finish/token progress), `ORCHICON_STALL_NO_FILE_DIFF_WINDOW` (default 180s — no file modifications), `ORCHICON_STALL_REPETITION_COUNT` (default 5 — same tool_call signature repeated within `ORCHICON_STALL_REPETITION_WINDOW`, default 300s). The worker's `budget_overrides.wall_clock_seconds` (default 3600) is the hard per-execution timeout backstop (context deadline → subprocess kill → recovery). Verification that exercises stall/timeout paths must use tight env windows + a free model.

### Docker / infrastructure changes

When a change modifies `deploy/compose/docker-compose.yml`, configs in `deploy/compose/`, or the telemetry setup in `internal/telemetry/`:

- **Full reset test**: Run `make nuke` then `make up` and wait for all containers to show `healthy`. This is the only reliable way to catch dependency ordering bugs, config regressions, and incorrect `depends_on` chains.
- **No-ZooKeeper check**: The stack must not contain a `zookeeper` service or container. Verify with `docker ps --filter name=orchicon-zookeeper` — the output should be empty. The ClickHouse config in `clickhouse-cluster.xml` must use `<keeper_server>` (embedded Keeper), never `<zookeeper>` pointing at an external host.
- **Control plane boot speed**: After `make build`, time how long it takes for `curl http://localhost:8080/healthz` to return `200`. With the non-blocking OTel gRPC dial (`grpc.NewClient`), this must be under 2 seconds even when the OTel collector is not yet healthy. Check the control plane logs for `"otel pipeline initialized"` — it should appear within milliseconds of process start, not after 10-20s.
- **Profile isolation**: `make up` must start exactly 6 containers (postgres, nats, clickhouse, signoz-schema-migrator, otel-collector, signoz). The stack must not contain a `zookeeper` service.

If the change adds a new API RPC, also verify the Connect endpoint responds (e.g. via `curl` or a frontend smoke test). If it adds a new table, verify the RLS gate still passes after migration.

**Do not claim "done" without having run the thing.** State what was verified and what was not in the commit message or PR description.

**Testing preference**: The canonical test is to build a release binary (`make build`) and install it like a user would. Do not rely on `go run` / `npx vite` for final verification unless the change is frontend-only and cannot be tested from a release bundle. If the change touches both layers, cut a release artifact and verify end-to-end from there.

## Dev Control Script

`scripts/dev.sh` is the one-command dev environment controller. It manages the full local stack — Docker Compose services (Postgres, NATS, SigNoz, OTel), the Go control plane, and the Vite frontend — so a new contributor can get everything running with a single command:

```
scripts/dev.sh start     # dev stack → migrations → control plane → frontend
scripts/dev.sh stop      # stop everything
scripts/dev.sh status    # show status of all components + endpoint checks
scripts/dev.sh restart   # stop then start
scripts/dev.sh logs      # tail control-plane + frontend logs
```

Or via Make: `make dev-start`, `make dev-stop`, `make dev-status`, `make dev-restart`, `make dev-logs`.

PID files and logs live in `.dev/` (gitignored).

When a phase adds a new runtime component — a reconciler, an adapter process, the recovery engine, the policy engine, a webhook dispatcher, etc. — update `scripts/dev.sh` so that `dev.sh start` brings it up and `dev.sh stop` tears it down.

## Install Scripts

`scripts/install.sh` (Linux/macOS) and `scripts/install.ps1` (Windows) are the one-line installers published at `orchicon.dev`:

```
curl -fsSL https://orchicon.dev/install | bash          # Linux/macOS
irm https://orchicon.dev/install.ps1 | iex               # Windows
```

They download the latest release binary from GitHub Releases, install it to `~/.local/bin` (or a chosen dir), and verify the install. The release workflow (`.github/workflows/release.yml`) builds binaries for linux/darwin/windows × amd64/arm64 on tag push and attaches them to the GitHub Release.

When a phase changes what ships in the binary — a new subcommand, a new dependency the binary needs at runtime, a new asset (e.g. the frontend bundle, adapter binaries, Rego policy files), or a new platform/architecture target — update the install scripts and the release workflow so the installer stays correct. Specifically:

- **`scripts/install.sh`** — update if the download asset name changes, new files need to be downloaded alongside the binary, or new post-install steps are required (e.g. installing an adapter).
- **`scripts/install.ps1`** — mirror any changes from `install.sh` for Windows. Both scripts must stay in sync.
- **`.github/workflows/release.yml`** — update the build matrix if a new OS/arch is added, add build steps if the binary now needs the frontend embedded, and verify the asset naming matches what the install scripts download.
- **README.md** — update the Installation section if the commands or prerequisites change.

Verify by running the installer against a draft release at minimum (`bash scripts/install.sh --version vX.Y.Z --dry-run` on each target platform, or `--uninstall` to test cleanup).

## Design Doc Index

| Doc | Subsystem |
|---|---|
| `docs/01_Architecture_Vision.md` | Tech direction, topology, principles |
| `docs/02_Domain_Model.md` | Entities, relationships, lifecycles |
| `docs/03_Scheduler_and_Runtime_Design.md` | Reconcilers, dispatch, health |
| `docs/04_Runtime_Adapter_SDK.md` | gRPC adapter contract, OpenCode |
| `docs/05_Worker_Specification.md` | Worker entity, permissions, budgets |
| `docs/06_Recovery_Workflow_Engine.md` | Triggers, recovery workflow, escalation |
| `docs/07_API_Specification.md` | Services, streaming, auth, webhooks |
| `docs/08_Event_Bus_and_Telemetry_Model.md` | NATS, OTel, SigNoz, events |
| `docs/09_Database_Schema.md` | Tables, outbox, RLS, migrations |
| `docs/10_Frontend_Architecture.md` | React, Connect-ES, workflow editor |

## Things you need to know

- **Landing page + install deploy**: `site/` holds the static landing page deployed to CloudFlare Pages (`orchicon-site`). The build step copies `scripts/install.{sh,ps1}` into the deployed bundle so the one-liner install commands work. `site/install` and `site/install.ps1` are git-ignored build artifacts. Full setup in `CLOUDFLARE_SETUP.md`.
- **Connect-ES codegen** is pinned to local v1 npm plugins (`protoc-gen-es` / `protoc-gen-connect-es`) matching the v1 runtime. `make gen` prepends `frontend/node_modules/.bin` to PATH. See PR #1 notes before bumping to v2.
- **Atlas RLS** policies are hand-appended SQL (the free tier does not diff `policy` blocks). After hand-editing a migration, run `make migrate-hash`. Future diffs won't drop RLS.
- **`orchicon dev`** subcommand embeds compose + migrations + frontend via `go:embed`. One-binary dev experience: compose up → wait healthy → migrate → serve. The OTel pipeline uses non-blocking `grpc.NewClient` so boot is <2s even without a healthy collector. NATS subscriber fans out events to streaming RPCs. Reconciler framework uses `pg_try_advisory_lock` for per-kind leadership.
- **Worker lifecycle**: draft → published → deprecated → retired. Published versions are immutable. WorkItem hierarchy: Epic → Feature → Task → Subtask (max 4 levels). Dependency edges form a DAG; cycle detection uses recursive CTE. Edit locks have automatic TTL expiry.
- **TaskReconciler** is the only component that creates WorkerExecutions. It polls ready tasks, resolves dependencies, selects a worker+adapter, and dispatches. The OpenCode adapter bridge wraps the `opencode` CLI as a subprocess. Simulation mode is opt-in only (`ORCHICON_SIMULATE_ADAPTER=1`) — real runtime calls with a free model are required for verification.
- **Workflows** are the top-level reconcilable object. The WorkflowReconciler progresses step DAGs, evaluating gates at transitions. Task steps create WorkItems and hand off to TaskReconciler. Frontend has a full drag-and-drop React Flow editor with undo/redo, cycle detection, palette with Workers, Work Items, Policies, and Step primitives.
- **Recovery** follows a default 6-step workflow (capture → summarize → preserve → review → plan → resume) with bounded auto-relax (25% / 150%) and L1→L2→L3 escalation. TaskReconciler triggers recovery on execution failure (opt-out, idempotent). Recovery is also available as typed work item kinds (stop, summarize_restart, human_escalation, retry_n).
- **Policy Engine** uses OPA v1 with bundles loaded from Postgres. Evaluation is narrowest-scope-first with first-definitive-decision-wins; default is allow (fail-open). Rego traces are captured for `ExplainDecision`.
- **Auth**: OIDC-based with built-in dev IdP for local verification (HS256). Production uses authorization-code flow. API keys are SHA-256 hashed with least-privilege scopes. RBAC is a per-RPC Connect interceptor. Frontend stores access tokens in memory; refresh tokens in HttpOnly cookies.
- **Webhooks**: NATS consumer dispatches events to matching subscriptions with HMAC signing, exponential backoff, and dead-letter queue (replayable). Runs in the control-plane binary.
- **BlobStore** has two backends: local filesystem (content-addressed, atomic writes, path-traversal-safe) and S3-compatible (AWS SDK v2).
- **Markdown** is supported on all prompt-affecting fields: work item description/AC, worker system_prompt, execution output/error/reasoning, composite prompt, project goals, and recovery narrative fields. The frontend uses `react-markdown` + `remark-gfm` via a reusable `<Markdown>` component with theme-aware styling. Server-side extraction of the `composite` field from JSONB prompt_context ensures the API delivers plain markdown text.

## UPDATES.md

> Read this before starting any work. All changes must be recorded in `UPDATES.md` in the same table format as the existing entries.

Phase progress and per-PR changes are tracked in `UPDATES.md` (created on the first run of the new AGENTS.md structure). Before commencing any work, always read `UPDATES.md` first to understand the current state and what has already been shipped.

When a PR is merged, add a new row to the table in `UPDATES.md` describing what was done. The table format is:

```
| # (next number) | Short description | done | Brief summary of what was shipped |
```

If no existing phase fits, group related PRs under a descriptive phase name (e.g. "Markdown rendering", "Prompt card fix"). Keep entries concise — one line per PR, linking the PR number where useful.
