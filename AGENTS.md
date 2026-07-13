# AGENTS.md — Development Guidelines for AI Agents

> This file is the entry point for any AI agent working on Orchicon.
> Read it before making any changes.
>
> **Start with § Implementation Progress** — it tells you what phases
> are done, in flight, and not started, so you know where you are.

## Project

- **Repo**: https://github.com/beardedparrott/Orchicon.git
- **Language**: Go (control plane) + TypeScript (frontend)
- **Design docs**: `docs/` — read the relevant doc before touching a subsystem

## Git Workflow

### Branching

- **Every major change gets a branch.** No commits directly to `main`.
- Branch naming: `<type>/<short-description>` (e.g. `feat/project-crud`,
  `fix/outbox-relay-dedup`, `chore/docker-compose-setup`).
- Types: `feat`, `fix`, `chore`, `refactor`, `docs`, `test`.

### Committing

- Commit early and often on your branch.
- Write clear commit messages in present tense:
  `Add project CRUD service and data-access layer`
- Stage only the files relevant to the commit. Never `git add -A` blindly.
- **Update § Implementation Progress in this file** when you complete or
  advance a phase. Keep the one-line "What landed" notes short and
  high-level — they orient the next agent, not replace commit history.

### Pull Requests

- When a major change is complete, push the branch and create a PR.
- PR title: same format as branch name (`feat: project CRUD service`).
- PR description should reference the design doc(s) it implements.
- After pushing, use `gh pr create` to open the PR.
- Do not merge your own PR without review unless explicitly told to.

### Sync

- Before starting work, always `git pull origin main` to get the latest.
- Before pushing, `git fetch origin && git rebase origin/main` if the
  branch has been open for a while.

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

Every piece of functionality built in this repo must follow these
security standards. They are the floor, not the ceiling — review them
when adding any new RPC, handler, or frontend form.

### Secrets & credentials

- **No secrets in code or commits.** DSNs, API keys, tokens, and
  passwords come from the environment (`internal/config`) or a secret
  store — never hardcoded, never committed. The `.env.example` file
  documents the variables without containing real values.
- **No secrets in logs.** Never log DSNs, tokens, passwords, or
  full request payloads that may carry credentials. The slog setup in
  `cmd/orchicon/main.go` logs structured fields; only log non-sensitive
  identifiers (tenant id, project id, trace id).
- **Hashed at rest.** API keys are hashed before storage (never
  plaintext). Passwords are never stored by the control plane (OIDC
  handles authentication). See docs/07 §6.1.
- **The dev stack credentials** in `deploy/compose/docker-compose.yml`
  (e.g. `orchicon:orchicon`) are local-dev-only placeholders. They must
  never appear in a production deployment config.

### Input validation & sanitization

- **Validate at the API boundary.** Every RPC handler validates and
  sanitizes input before it reaches the data-access layer. See
  `internal/project/validate.go` for the pattern: trim, bound-check
  length, regex-validate structured fields (e.g. slug), and reject
  malformed data with `connect.CodeInvalidArgument`.
- **Parameterized queries only.** All SQL uses pgx parameterized
  queries (`$1`, `$2`, …). No string interpolation of user input into
  SQL, ever. The data-access layer (`internal/db`) is the only place
  SQL lives (invariant #4).
- **JSON fields are validated.** JSON-typed columns (e.g. `goals`)
  must be parsed/validated as valid JSON before storage. Reject
  malformed JSON at the handler, not the database.
- **Size bounds on all inputs.** Every text input has a max length
  enforced at the handler to prevent memory-exhaustion abuse.
- **Slugs and identifiers are regex-validated.** Slugs match
  `^[a-z0-9]+(?:-[a-z0-9]+)*$`; IDs are ULIDs generated server-side,
  never accepted from the client on create.

### Tenant isolation

- **Every request is tenant-scoped.** The middleware resolves the
  tenant and the data-access layer sets `app.tenant_id` per
  transaction. RLS is the backstop — even a buggy query cannot leak
  cross-tenant data (docs/09 §8.5).
- **No cross-tenant queries.** The data-access layer injects
  `tenant_id` into every `WHERE` and `INSERT`. A query without a
  tenant scope is a bug, not an optimization.

### Frontend

- **The browser never stores long-lived secrets.** Access tokens live
  in memory; refresh tokens in HttpOnly secure cookies (docs/10 §7).
  API keys are for headless/CI clients only.
- **Client-side validation is UX, not security.** Zod schemas in forms
  improve the user experience but every rule is re-validated
  server-side. Never trust client-side validation as the security gate.
- **No business logic in the frontend** (invariant #1). The UI does
  not make policy, scheduling, or recovery decisions.

## Tooling hints

- When you need library docs (Connect-ES, Atlas, TanStack Router, pgx,
  NATS, SigNoz), use `context7` tools before guessing.
- If unsure how to use a library or pattern, use `gh_grep` to search
  real GitHub usage examples.
- LSP servers (gopls, typescript, eslint, yaml-ls) are enabled —
  diagnostics surface in the edit loop. Treat them as fast feedback;
  `make ci` is the authoritative gate.

## Verification

> **Compilation passing is not the same as working.**
> Agents must verify runtime behavior, not just `go build` / `tsc`.

Before marking a phase or task as complete, verify the following at
minimum (adapt to what the change touches):

1. **`make ci` passes end-to-end** — buf lint, codegen, go vet/test,
   RLS gate. This is the authoritative CI gate.
2. **Dev stack starts healthy** — `make up` then `make ps` shows all
   containers `healthy` (not just `running`).
3. **Migrations apply cleanly** — `make migrate` against the compose
   Postgres; `make rls-check` passes.
4. **Control plane boots and serves** — `make build && make run`, then
   `curl http://localhost:8080/healthz` returns `{"status":"ok"}`.
5. **Frontend renders** — `make fe-dev` (or `npx vite`), then
   `curl http://localhost:5173/` returns HTTP 200 with the app shell.

If the change adds a new API RPC, also verify the Connect endpoint
responds (e.g. via `curl` or a frontend smoke test). If it adds a new
table, verify the RLS gate still passes after migration.

**Do not claim "done" without having run the thing.** State what was
verified and what was not in the commit message or PR description.

## Dev Control Script

`scripts/dev.sh` is the one-command dev environment controller. It
manages the full local stack — Docker Compose services (Postgres, NATS,
SigNoz, OTel), the Go control plane, and the Vite frontend — so a new
contributor can get everything running with a single command:

```
scripts/dev.sh start     # dev stack → migrations → control plane → frontend
scripts/dev.sh stop      # stop everything
scripts/dev.sh status    # show status of all components + endpoint checks
scripts/dev.sh restart   # stop then start
scripts/dev.sh logs      # tail control-plane + frontend logs
```

Or via Make: `make dev-start`, `make dev-stop`, `make dev-status`,
`make dev-restart`, `make dev-logs`.

PID files and logs live in `.dev/` (gitignored).

### Every phase MUST update this script

When a phase adds a new runtime component — a reconciler, an adapter
process, the recovery engine, the policy engine, a webhook dispatcher,
etc. — **update `scripts/dev.sh`** so that `dev.sh start` brings it up
and `dev.sh stop` tears it down. A phase is not complete if the dev
script does not manage its components. Specifically:

- Add the component to `start_*` (build, launch, wait-for-ready).
- Add the component to `do_stop` (PID file cleanup, graceful shutdown).
- Add the component to `do_status` (running check + endpoint probe).
- Add the component to `do_logs` if it has a log file.

This keeps the dev experience reproducible: one script, one command,
the whole system up or down.

## Install Scripts

`scripts/install.sh` (Linux/macOS) and `scripts/install.ps1` (Windows)
are the one-line installers published at `orchicon.dev`:

```
curl -fsSL https://orchicon.dev/install | bash          # Linux/macOS
irm https://orchicon.dev/install.ps1 | iex               # Windows
```

They download the latest release binary from GitHub Releases, install
it to `~/.local/bin` (or a chosen dir), and verify the install. The
release workflow (`.github/workflows/release.yml`) builds binaries for
linux/darwin/windows × amd64/arm64 on tag push and attaches them to
the GitHub Release.

### Every phase MUST update these scripts

When a phase changes what ships in the binary — a new subcommand, a
new dependency the binary needs at runtime, a new asset (e.g. the
frontend bundle, adapter binaries, Rego policy files), or a new
platform/architecture target — **update the install scripts and the
release workflow** so the installer stays correct. A phase is not
complete if the installer does not work end-to-end. Specifically:

- **`scripts/install.sh`** — update if the download asset name changes,
  new files need to be downloaded alongside the binary, or new
  post-install steps are required (e.g. installing an adapter).
- **`scripts/install.ps1`** — mirror any changes from `install.sh`
  for Windows. Both scripts must stay in sync.
- **`.github/workflows/release.yml`** — update the build matrix if a
  new OS/arch is added, add build steps if the binary now needs the
  frontend embedded, and verify the asset naming matches what the
  install scripts download.
- **README.md** — update the Installation section if the commands or
  prerequisites change.

Verify by running the installer against a draft release at minimum
(`bash scripts/install.sh --version vX.Y.Z --dry-run` on each target
platform, or `--uninstall` to test cleanup).

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

## Implementation Progress

> **This is the high-level "where are we" tracker.**
> Phases mirror the vertical-slice build order in
> `docs/01_Architecture_Vision.md` §9. Update this table whenever you
> complete, advance, or scope-change a phase — keep the notes to one or
> two high-level lines per phase (commit history has the detail).
>
> Status values: `not started` · `in progress` · `done`.

| # | Phase | Status | What landed |
|---|---|---|---|
| 1 | Foundation | done | Go module + binary skeleton (`cmd/orchicon`, `internal/`); Protobuf schema (`orchicon.api.v1`, `orchicon.adapter.v1`); Connect codegen (Go + TS); Atlas migrations for tenants/identities/projects with RLS + CI gate; Docker Compose (Postgres, NATS, SigNoz, OTel); Makefile; Vite+React+TS shell with Connect-ES, TanStack Router, Tailwind+shadcn/ui |
| 2 | Projects slice | done | Project CRUD (Create/Get/List/Update/Archive) full stack: Go handler + data-access layer with pgx + tenant scoping + RLS backstop; Connect handler wiring; transactional outbox with NATS JetStream relay; frontend project list + detail + create form (React Hook Form + Zod + TanStack Query) |
| 3 | Realtime + infrastructure | done | `orchicon dev` subcommand (embeds compose + migrations + frontend via go:embed); outbox relay lag metrics (`orchicon_outbox_lag`); reconciler framework (work queue + advisory-lock leadership + manager); OTel pipeline (tracer/meter/exporter → SigNoz); StreamProjectEvents server-stream RPC fanning out from NATS; trace propagation via Connect headers; `useStream` hook (reconnect + backoff + dedup + resume) + live event feed on project detail page |
| 4 | Workers + WorkItems | done | WorkerService (CreateWorker, PublishWorkerVersion, DeprecateWorker, RetireWorker, GetWorker, ListWorkers, ListWorkerVersions) + edit locks (TTL, heartbeat, visual editor); WorkItemService (CRUD, AddDependency/RemoveDependency with recursive-CTE cycle rejection, GetDependencyGraph, AssignWorker) with CAS optimistic concurrency + outbox events; Atlas migrations (workers, worker_versions, work_items, work_item_dependencies, edit_locks) with RLS; frontend worker catalog + version history + create form (system_prompt template vars, permissions, gated_tools, budget overrides) + work item tree view + Kanban board + dependency graph (read-only React Flow) + edit lock banner on worker editor |
| 5 | Scheduling + adapters | done | TaskReconciler (dependency resolution via recursive CTE, rule-based worker/adapter selection, dispatch flow with CAS status transitions); RuntimeAdapterService (orchicon.adapter.v1: Register/Heartbeat/Execute bistream) + public RuntimeAdapterService (ListAdapters, GetAdapterCapabilities); ExecutionService (Get/List/StreamExecutionEvents via NATS fan-out, Pause/Resume/Cancel/CheckpointNow, ApproveToolCall Tier 2 per-tool-call gating); OpenCode adapter bridge (CLI subprocess wrapper, stdout JSON → telemetry events, simulation mode for dev); Atlas migrations (runtime_adapters, worker_executions, checkpoints) with RLS; frontend execution live view (streaming telemetry, manual controls), tool-call approval dialog, adapter registry |
| 6 | Workflows | not started | Workflow CRUD, step DAG, runs + frontend visual drag-and-drop editor (React Flow) |
| 7 | Recovery + Policy | not started | Recovery Engine, Rego Policy Engine + frontend recovery timeline, policy editor |
| 8 | Telemetry + Cost | not started | OTel pipeline, SigNoz integration, cost attribution + frontend SigNoz embedding, cost explorer |
| 9 | Auth + Webhooks + Polish | not started | OIDC, API keys, RBAC, webhooks, edit locks + frontend auth flow, end-to-end integration |

### Cross-cutting notes

- **Connect-ES codegen** is pinned to local v1 npm plugins
  (`protoc-gen-es` / `protoc-gen-connect-es`) matching the v1 runtime.
  `make gen` prepends `frontend/node_modules/.bin` to PATH. See PR #1
  notes before bumping to v2.
- **Atlas RLS** policies are hand-appended SQL (the free tier does not
  diff `policy` blocks). After hand-editing a migration, run
  `make migrate-hash`. Future diffs won't drop RLS.
- **Phase 3**: the `orchicon dev` subcommand embeds the compose stack,
  migrations, and frontend bundle via `go:embed` (assets.go at the
  module root). `orchicon dev start` is the complete one-binary dev
  experience: compose up → wait healthy → migrate → serve (control
  plane + embedded frontend). `scripts/dev.sh` delegates to `orchicon
  dev` when the binary is available. The OTel pipeline exports to the
  OTel collector at `cfg.OTelEndpoint`; the NATS subscriber
  (`eventbus.NATSSubscriber`) fans out events to streaming RPCs; the
  reconciler framework (`reconciler.Manager`) provides per-kind
  leadership via `pg_try_advisory_lock`. The frontend `useStream` hook
  wraps Connect-ES server streams with reconnect + backoff + dedup +
  resume.
- **Phase 4**: WorkerService implements the full worker lifecycle
  (draft → published → deprecated → retired — docs/05 §4) with
  versioned snapshots (`worker_versions` table). A published version is
  immutable; changes require a new version. WorkItemService implements
  the work hierarchy (Epic → Feature → Task → Subtask, max 4 levels)
  with dependency edges as a DAG. Cycle detection uses a recursive CTE
  on `work_item_dependencies` (docs/09 §11) — `CheckCycleWithRecursiveCTE`
  traverses forward from the target and rejects the edge if the source
  is reachable. Edit locks (`edit_locks` table) prevent concurrent edits
  in the visual Worker editor (docs/07 §3.3); they expire automatically
  on TTL and are acquired/released via `AcquireEditLock`/
  `ReleaseEditLock`. The frontend uses React Flow for the read-only
  dependency graph, TanStack Query for server state, and React Hook
  Form + Zod for the worker create form (system_prompt template vars,
  permissions, gated_tools, budget overrides).
- **Phase 5**: The TaskReconciler is the only component permitted to
  create WorkerExecutions (docs/03 §8 invariant #1). It polls ready
  tasks, checks dependencies via a recursive CTE
  (`CheckDependenciesSatisfied`), selects a Worker by rule-based
  ranking (published + health + concurrency), selects an Adapter by
  kind + heartbeat freshness + free capacity, and dispatches via the
  AdapterBridge interface with CAS status transitions
  (ready→assigned→running). The OpenCode adapter bridge wraps the
  `opencode` CLI as a subprocess, parsing stdout JSON lines into
  telemetry events; if the binary is absent, it runs in simulation
  mode for dev verification (docs/04 §6.3). The ExecutionService
  streams events from NATS (`orchicon.events.execution.>`), provides
  manual controls (Pause/Resume/Cancel/CheckpointNow), and routes
  Tier 2 per-tool-call approvals (docs/05 §7.1) via an in-memory
  approval registry. The dev server seeds an in-process OpenCode
  adapter (`adp_opencode_dev`) on boot so the TaskReconciler has a
  ready adapter for dispatch. The next step (Phase 6) adds Workflow
  CRUD, step DAG, runs + frontend visual drag-and-drop editor.
