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

- **Every change gets a branch.** No commits directly to `main`.
- Branch naming: `<type>/<short-description>` (e.g. `feat/project-crud`,
  `fix/outbox-relay-dedup`, `chore/docker-compose-setup`).
- Types: `feat`, `fix`, `chore`, `refactor`, `docs`, `test`.
- **Create the branch before starting work**, not after. The PR
  should be opened by the agent for review — do not wait to be asked.

### Committing

- Commit early and often on your branch.
- Write clear commit messages in present tense:
  `Add project CRUD service and data-access layer`
- Stage only the files relevant to the commit. Never `git add -A` blindly.
- **Update § Implementation Progress in this file** when you complete or
  advance a phase. Keep the one-line "What landed" notes short and
  high-level — they orient the next agent, not replace commit history.

### Pull Requests

- When work on a change is complete, push the branch and create a PR.
- PR title: same format as branch name (`feat: project CRUD service`).
- PR description should reference the design doc(s) it implements.
- After pushing, use `gh pr create` to open the PR.
- **All PRs must carry the `release` label** during early development.
  The `auto-release.yml` workflow creates a GitHub release automatically
  when a labeled PR is merged to main (bumps the patch version). Remove
  this requirement when a more selective release cadence is desired.
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
    containers `healthy` (not just `running`). When the change touches
    Docker or infrastructure:
    - Check that ZooKeeper is NOT listed in `make ps` output.
    - Verify all 6 containers (postgres, nats, clickhouse,
      signoz-schema-migrator, otel-collector, signoz) show up.
    - Run `make nuke` then `make up` from a clean slate to verify the
      full startup sequence works end-to-end.
3. **Migrations apply cleanly** — `make migrate` against the compose
   Postgres; `make rls-check` passes.
4. **Control plane boots and serves** — `make build && make run`, then
   `curl http://localhost:8080/healthz` returns `{"status":"ok"}`.
   Time this command — if the telemetry stack is starting, the boot
   should still be <2s (not 20s+). Check the control plane logs for
   `"otel pipeline initialized"` — if it appears before the 2s mark,
   the non-blocking OTel dial is working.
5. **Frontend renders** — `make fe-dev` (or `npx vite`), then
   `curl http://localhost:5173/` returns HTTP 200 with the app shell.
6. **Runtime calls are real, not simulated** — end-to-end verification
   that exercises adapter dispatch MUST call the real `opencode` runtime
   with a **free model** (e.g. `opencode/deepseek-v4-flash-free`), never
   the simulation-mode fallback. Simulation mode is a development aid for
   the offline case only; it must not be used to "verify" dispatch,
   recovery, or any flow that depends on adapter telemetry. If
   `opencode` is absent from PATH, fix the environment (install it) —
   do not fall back to simulation and claim the slice works. Seed
   workers / executions used for verification must pin a free model in
   `model_ref` so verification is reproducible at no cost.
   - **Stall + wall-clock guardrails** (docs/06 §2 triggers): the
     opencode adapter bridge runs a per-execution progress monitor that
     detects stuck-looping and triggers recovery (opt-out, idempotent).
     Three stall signals, configurable via env:
     `ORCHICON_STALL_NO_PROGRESS_WINDOW` (default 120s — no
     step_finish/token progress), `ORCHICON_STALL_NO_FILE_DIFF_WINDOW`
     (default 180s — no file modifications), `ORCHICON_STALL_REPETITION_COUNT`
     (default 5 — same tool_call signature repeated within
     `ORCHICON_STALL_REPETITION_WINDOW`, default 300s). The worker's
     `budget_overrides.wall_clock_seconds` (default 3600) is the hard
     per-execution timeout backstop (context deadline → subprocess
     kill → recovery). Verification that exercises stall/timeout paths
     must use tight env windows + a free model.

### Docker / infrastructure changes

When a change modifies `deploy/compose/docker-compose.yml`, configs in
`deploy/compose/`, or the telemetry setup in `internal/telemetry/`:

- **Full reset test**: Run `make nuke` then `make up` and
  wait for all containers to show `healthy`. This is the only reliable
  way to catch dependency ordering bugs, config regressions, and
  incorrect `depends_on` chains.

- **No-ZooKeeper check**: The stack must not contain a `zookeeper`
  service or container. Verify with `docker ps --filter
  name=orchicon-zookeeper` — the output should be empty. The
  ClickHouse config in `clickhouse-cluster.xml` must use
  `<keeper_server>` (embedded Keeper), never `<zookeeper>` pointing
  at an external host.

- **Control plane boot speed**: After `make build`, time how long it
  takes for `curl http://localhost:8080/healthz` to return `200`. With
  the non-blocking OTel gRPC dial (`grpc.NewClient`), this must be
  under 2 seconds even when the OTel collector is not yet healthy.
  Check the control plane logs for `"otel pipeline initialized"` — it
  should appear within milliseconds of process start, not after 10-20s.

- **Profile isolation**: `make up` must start exactly 6 containers
  (postgres, nats, clickhouse, signoz-schema-migrator, otel-collector,
  signoz). The stack must not contain a `zookeeper` service.

If the change adds a new API RPC, also verify the Connect endpoint
responds (e.g. via `curl` or a frontend smoke test). If it adds a new
table, verify the RLS gate still passes after migration.

**Do not claim "done" without having run the thing.** State what was
verified and what was not in the commit message or PR description.

**Testing preference**: The canonical test is to build a release binary
(`make build`) and install it like a user would. Do not rely on
`go run` / `npx vite` for final verification unless the change is
frontend-only and cannot be tested from a release bundle. If the change
touches both layers, cut a release artifact and verify end-to-end from
there.

### Token discipline

The project's model spend is rising. Be economical:

- Prefer parallel tool calls when independent (one message, many
  tools) to cut round-trips.
- Read only the slice of a file you need; avoid re-reading whole files.
- Keep edits surgical — match surrounding style, don't reflow untouched
  code.
- Skip preamble/postamble in responses; the diff speaks for itself.
- Run `make ci` once at the end, not after every edit.

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
| 6 | Workflows | done | WorkflowService (CreateWorkflow, UpdateWorkflowVersion, PublishWorkflow, DeprecateWorkflow, GetWorkflow, ListWorkflows, ListWorkflowVersions, StartWorkflow, AbortWorkflow, GetWorkflowRun, ListWorkflowRuns, GetWorkflowStepRuns, StreamWorkflowEvents, AcquireEditLock/ReleaseEditLock/GetEditLock) + WorkflowReconciler (step DAG progression, gate evaluation — Rego pass-through pending Phase 7, task-step→WorkItem handoff to TaskReconciler) + step types (task/decision/approval/parallel/recover); Atlas migrations (workflows, workflow_versions, workflow_runs, workflow_step_runs) with RLS; frontend full visual drag-and-drop React Flow editor (draggable Worker tiles, gate/decision/approval/parallel/recover step nodes, wire connections, inline property editing, undo/redo, client-side cycle-detection validation, edit lock banner) + workflow run view (live step-transition overlay on canvas, streaming event feed) + list + create form; manager scan pass (Reconcile with empty key) so both reconcilers discover work; TaskReconciler transitions WorkItem to succeeded/failed on execution result; dev adapter heartbeat renewal |
| 7 | Recovery + Policy | done | PolicyService (CreatePolicy, PublishPolicy, SupersedePolicy, GetPolicy, ListPolicies, ListPolicyVersions, UpdatePolicyVersion, EvaluatePolicy dry-run, ExplainDecision by decision_id/trace_id, ListDecisions, GetDecision) + Rego Policy Engine (OPA v1: bundle loading from Postgres, narrowest-scope-first + first-definitive-decision-wins, Rego trace capture for ExplainDecision, CompileModule at publish, EvaluateGate at the dispatch decision point — wired into WorkflowReconciler gate, fail-open governance floor); RecoveryService (TriggerRecovery, CancelRecovery, GetRecovery, ListRecoveries, GetRecoveryStepRuns, StreamRecoveryEvents, ApproveContinuationPlan, RejectContinuationPlan, GetContinuationPlan, MarkTaskSucceeded) + RecoveryReconciler (default 6-step workflow: capture→summarize→preserve→review→plan→resume; checkpoint vs summarize-resume selection; bounded auto-relax 25%/150% thresholds; escalation L1→L2→L3; Reviewer/human task completion); TaskReconciler triggers recovery on execution failure (opt-out, idempotent); Atlas migrations (policies, policy_versions, policy_decisions, recovery_executions, recovery_step_runs, continuation_plans) with RLS; opencode adapter simulation mode now explicit opt-in (ORCHICON_SIMULATE_ADAPTER=1) — no silent fallback (real runtime calls with free model); frontend policy editor (Rego module + decision point/scope/effect + dry-run test pane + decision log) + rich recovery timeline (full narrative why/what/how/where/when per step, continuation-plan approval, MarkTaskSucceeded, live event feed) + lists + create form; verified end-to-end with real opencode dispatch on opencode/deepseek-v4-flash-free (recovery progressed to RESUMED) |
| 8 | Telemetry + Cost | done | OTel pipeline finalized with `correlation_id` propagation (baggage + span attribute) across API→reconciler→adapter→AI Gateway (docs/08 §3, §5.1); middleware generates + echoes `x-orchicon-correlation-id`; TelemetryService (QueryTraces/Metrics/Logs proxy to SigNoz/ClickHouse with tenant-scoped filters injected from context, StreamTelemetry fans out usage events from NATS, GetDashboard custom Orchicon roll-up); AIGatewayService (ListProviders, GetUsage, GetCost with drill-down roll-up Tenant→Project→Task→Execution, StreamUsageEvents); usage_records dual-write (Postgres source of truth + OTel metrics `orchicon_tokens_consumed`/`orchicon_cost_usd` → ClickHouse) recorded by the opencode adapter on `step_finish`; Atlas migration (usage_records) with RLS; config `ORCHICON_SIGNOZ_URL`; frontend telemetry hub (tabbed: Overview dashboard + custom cost explorer with Tenant→Project→Task→Execution drill-down + embedded SigNoz traces/metrics/logs via same-origin `/signoz` proxy — seamless embedding inside the Orchicon shell) |
| 9 | Auth + Webhooks + Polish | done | OIDC auth (dev IdP HS256 tokens + production code-flow via coreos/go-oidc) + token refresh (HttpOnly cookie); API keys hashed at rest (SHA-256) with least-privilege scoped entitlements + rotation; RBAC middleware (per-RPC Connect interceptor mapping procedures to resource:action entitlements, admin bypass); AuthService + WebhookService protos; Atlas migrations (roles, role_bindings, api_keys, event_subscriptions, webhook_deliveries + identity_type column) with RLS; WebhookService Connect handler (create/get/list/update/delete/test subscriptions, list deliveries, replay dead-letter, stream) + NATS consumer dispatcher (HTTP POST + HMAC signing + exponential backoff + dead-letter); BlobStore abstraction (local filesystem — production-viable: content-addressed + atomic writes + path-traversal-safe; S3-compatible); deployment-mode validation (local/production — production enforces real OIDC + signing key); frontend auth flow (in-memory access token + refresh-on-401 interceptor, /login dev+OIDC, /auth/callback, session bootstrap, RBAC-gated nav + RequireEntitlement); admin views (tabbed: tenants, identities, roles, API keys w/ one-time plaintext + rotate/revoke, audit); webhook subscription management + deliveries; verified end-to-end: dev-login → session → authed RPCs → scoped API key denied project:write (403) → webhook CRUD → token refresh |

- **Theme system**: The frontend now ships 20 themes (10 light + 10 dark) with a data-attribute-driven CSS variable system (`[data-theme="..."].dark`). Dark backgrounds use 7-10% lightness (not 3.9% pitch black). A `ThemeProvider` reads from a Zustand store persisted to `localStorage`. The topbar has a sun/moon toggle button + palette icon linking to `/preferences`, which shows all 20 themes as visual cards with color swatches.
- **Infra consolidation**: The Docker Compose stack dropped from 7 to 6 containers by replacing the separate ZooKeeper container with ClickHouse's built-in Keeper (embedded RAFT-based coordination — enabled via `<keeper_server>` in `clickhouse-cluster.xml`). The `depends_on` chain was relaxed: the schema migrator, OTel collector, and SigNoz query-service all start asynchronously instead of blocking sequentially. The control plane's OTel gRPC connection now uses `grpc.NewClient` (non-blocking dial) instead of the blocking `WithEndpoint`, eliminating the 20-second startup delay when the OTel collector is still initializing. These changes cut container-to-healthy time and let the app become usable before telemetry is fully ready.

### Cross-cutting notes

- **Landing page + install deploy**: `site/` holds the static landing
  page (hero with one-liner install, quick start, what-it-is grid,
  options table, build-from-source, CTA). It is deployed to
  CloudFlare Pages as project `orchicon-site`, bound to the custom
  domain `orchicon.dev` (configured in the CF dashboard; no DNS
  records or wrangling needed — Pages handles the cert). The build
  step (`scripts/build-site.sh`, referenced by `wrangler.toml`) copies
  the canonical install scripts from `scripts/install.{sh,ps1}` into
  the deployed bundle, so
  `curl -fsSL https://orchicon.dev/install | bash` and
  `irm https://orchicon.dev/install.ps1 | iex` are served directly
  from the Pages project — no CloudFlare redirect rules. Single source
  of truth for the installers stays in `scripts/`. When a script
  changes, push to `main` — CloudFlare Pages auto-deploys within a
  minute and the new version is live. `site/install` and
  `site/install.ps1` are git-ignored build artifacts, never edited by
  hand. Full setup recipe + troubleshooting lives in
  `CLOUDFLARE_SETUP.md`. (Phase 10 placeholder until a real release /
  launch site lands.)
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
- **Phase 6**: Workflows are the top-level reconcilable object for
  execution; tasks are reconciled as children (docs/02 §2.4). The
  WorkflowReconciler progresses a run's step DAG: pending steps whose
  `depends_on` are all satisfied transition to ready; ready steps
  evaluate their `gate_policy_ref` (Rego pass-through for v0.1 — the
  Policy Engine lands in Phase 7) then dispatch by kind. Task steps
  create a WorkItem (kind=task) with the step's Worker ref and hand it
  to the TaskReconciler for dispatch (only the TaskReconciler creates
  WorkerExecutions — docs/03 §8 invariant #1); the step run polls the
  WorkItem to completion. Decision/parallel/recover steps complete
  immediately; approval steps block at `approval_pending` (human
  approval wiring arrives with the Policy engine). The reconciler
  manager gained a scan pass (`Reconcile(ctx, "")` when the work queue
  is empty) so both the TaskReconciler and WorkflowReconciler discover
  work without an explicit enqueue path. The TaskReconciler now
  transitions the linked WorkItem to succeeded/failed when an execution
  terminates (`OnResult`), closing the loop for workflow task-step
  polling. The dev server renews the in-process adapter heartbeat every
  30s so dispatch works beyond the 60s heartbeat TTL. The visual editor
  is a full React Flow drag-and-drop canvas (docs/10 §11): draggable
  Worker tiles + gate/decision/approval/parallel/recover step nodes,
  wired connections (= `depends_on`), inline property editing,
  undo/redo (Ctrl+Z / Ctrl+Shift+Z), and client-side cycle-detection
  validation. The run view overlays live step transitions on the same
  canvas via `StreamWorkflowEvents`.
- **Phase 7**: The Rego Policy Engine (OPA v1) evaluates published
  Policies at decision points (admission/dispatch/budget/approval/
  recovery/completion — docs/02 §2.5 Tier 1). It loads policies on-demand
  from Postgres (the Rego modules live in `policy_versions`); a compiled-
  bundle mode is a v0.2 optimization. Evaluation order is narrowest-
  scope-first (task > worker > project > tenant) then first-definitive-
  decision-wins; when no published policy matches, the default is allow
  (governance floor). Each evaluation captures the Rego trace
  (`topdown.BufferTracer`) persisted as a `policy_decision` row so
  `ExplainDecision` can return it. `CompileModule` validates Rego at
  publish time. The WorkflowReconciler gate now calls `EvaluateGate` (the
  dispatch decision point); fail-open on error. The Recovery Workflow
  Engine (docs/06) is a `RecoveryReconciler` (registered with the
  manager — 3 reconcilers now) that progresses recoveries through the
  default 6-step workflow (capture→summarize→preserve→review→plan→
  resume). The TaskReconciler triggers recovery on execution failure via
  the `RecoveryTrigger` interface (loose coupling — no scheduler→recovery
  import): on failure, the task transitions to `recovering` and
  `TriggerOnFailure` runs (idempotent — docs/06 §9). Resumption path
  selection (docs/06 §4): direct checkpoint replay when a checkpoint
  exists, else summarize-resume. Bounded auto-relax (docs/06 §11): up to
  +25% automatically, >150% blocks at L3 for human approval. Escalation
  L1→L2→L3 (docs/06 §7). `MarkTaskSucceeded` allows the Reviewer Worker
  (during recovery) or a human to mark a Task succeeded (docs/02 §4 #2).
  The opencode adapter's simulation mode is now explicit opt-in
  (`ORCHICON_SIMULATE_ADAPTER=1`, offline dev only) — NOT a silent
  fallback (AGENTS.md verification: real runtime calls with a free model).
  Verified end-to-end: a real opencode dispatch on
  `opencode/deepseek-v4-flash-free` + a triggered recovery progressed
  through all 6 steps to `RECOVERY_STATUS_RESUMED`, with the full timeline
  narrative (why/what/how/where/when per step) visible in the UI.
- **Phase 8**: The OTel pipeline now propagates a `correlation_id` across
  the whole user action (docs/08 §3, §5.1): the API middleware extracts
  it from baggage or generates one, records it as a span attribute, and
  echoes it back via the `x-orchicon-correlation-id` response header so
  clients can join logs/telemetry to the originating request. Downstream
  spans (reconciler, adapter, gateway) inherit it via the propagated
  context — `telemetry.StartSpan` / `RecordCorrelation` are the helpers.
  TelemetryService proxies tenant-scoped queries to SigNoz/ClickHouse
  (docs/07 §3.9): the `tenant_id` filter is injected from the request
  context, never trusted from the client (AGENTS.md tenant isolation).
  When the SigNoz backend is unreachable, query methods return
  `degraded=true` rather than erroring (docs/08 §8: telemetry loss never
  blocks control flow). The frontend embeds the SigNoz UI same-origin
  under `/signoz` (Vite proxy in dev) so it lives inside the Orchicon
  shell — same auth, same visual language (docs/10 §11). The AI Gateway
  (embedded in the control plane binary — docs/01 §2) records LLM usage
  from the opencode adapter's `step_finish` events: the adapter calls a
  `UsageRecorderFunc` (decoupled via a function type so the adapter has
  no import dependency on the gateway) which performs the dual-write —
  Postgres `usage_records` (source of truth, RLS) + OTel metrics
  `orchicon_tokens_consumed` / `orchicon_cost_usd` → ClickHouse via the
  OTel collector (docs/08 §5.2). Cost attribution rolls up
  Tenant→Project→Task→Execution (GetCost with `UsageRollup` granularity).
  Note: the zero-time window sentinel in the data-access layer uses
  `<= 'epoch'::timestamptz` so Go's `time.Time{}` (year 1) is treated as
  "no bound" — `'epoch'` alone did not match year 1 and silently excluded
  all rows.
- **Phase 9**: Auth is OIDC-based with a built-in dev IdP for local
  verification. In local mode (`ORCHICON_OIDC_ISSUER=local`) the control
  plane mints short-lived HS256 access tokens + refresh tokens itself
  — the full auth flow (login → session → authed RPCs → refresh) is
  verifiable locally with no external identity provider
  (AGENTS.md verification). In production mode the control plane runs
  the OIDC authorization-code flow (coreos/go-oidc) against a configured
  issuer, verifies ID tokens, upserts the identity, and issues its own
  access tokens thereafter (docs/07 §6.1). The access token lives in
  memory (frontend); the refresh token in an HttpOnly secure cookie
  (docs/10 §7). The Connect-ES transport interceptor injects the bearer
  token and transparently refreshes on 401 (shared refresh guard avoids
  a storm). API keys are hashed at rest (SHA-256 — appropriate for
  high-entropy keys; bcrypt is for human passwords, which Orchicon
  never stores). API keys are least-privilege: the key's own scopes ARE
  the effective entitlement set, never an admin (a machine credential
  must declare exactly what it may do — unioning the bound identity's
  role entitlements would widen the key beyond its declared scope).
  RBAC is a per-RPC Connect interceptor (`rbac.EntitlementFor`
  maps `/<package>.<Service>/<Method>` → `resource:action`; read RPCs by
  convention need `resource:read`, mutations `resource:write` unless a
  granular action like `worker:publish` is declared). Admins (identity
  bound to the `admin` role) bypass per-call checks; the dev flow binds
  the admin role to the dev identity on first login so the dev user has
  full access. UI gating (`RequireEntitlement`, `useIsAdmin`) hides
  affordances the identity cannot perform — it is UX only; the server
  is the security boundary (docs/10 §10 invariant #5). The webhook
  dispatcher is a NATS consumer that fans out events from the
  `ORCHICON_EVENTS` stream to matching subscriptions, POSTs with HMAC
  signing + exponential backoff retries (2^n s, capped at 5 min), and
  dead-letters events that exceed the retry budget (replayable via
  `ReplayDelivery`). It runs inside the control-plane binary (started
  in `server.Run`), so `scripts/dev.sh` already manages it — no new
  process. The BlobStore ships two production backends: local filesystem
  (content-addressed, atomic temp+rename writes, path-traversal-safe)
  and S3-compatible (AWS SDK v2, path-style so MinIO works). Deployment
  mode (`ORCHICON_MODE`) validates on boot: production requires a real
  OIDC issuer (not `local`) and a non-default signing key — the local
  dev defaults are rejected. Verified end-to-end: dev-login → session →
  authed ListProjects → scoped API key (`project:read`) denied
  `project:write` with HTTP 403 → webhook subscription CRUD → token
  refresh via the HttpOnly cookie.
- **Search, sort, filter, and hard delete**: List RPCs for projects, workers, and
  workflows now support `search` (ILIKE on name/slug/purpose), `status` filter,
  `sort_by`, and `sort_order` params. Each entity also has a new `Delete*` RPC
  that hard-deletes with application-level cascade cleanup (work items, workflow
  versions, runs, step runs, edit locks, usage records, outbox events). Frontend
  list pages have search bars, native `<select>` status/sort controls; detail
  pages have destructive Delete buttons with `window.confirm` guards. Build
  verified: `go build ./...` and `npm run build` pass cleanly.
- **Full edit capabilities across all entities**: Projects now editable inline
  (UpdateProject RPC wired to detail page with name/slug edit). Work items get
  full inline editing (title, description, acceptance criteria, priority,
  context window, status). Webhook subscriptions editable (target URL, event
  filter). Name length limit raised globally from 200 to 500 characters across
  projects, workers, workflows, policies (backend validation + frontend Zod
  schemas). Build verified: `go build ./...`, `npm run build` pass cleanly.
- **Project lifecycle**: New `ActivateProject` RPC transitions drafting→active.
  Work items and workflows now reject creation under non-active projects
  (db.RequireProjectActive guard). Frontend has an Activate button on the
  project detail page when status is drafting.
- **Worker form revamp**: The worker create form replaces raw JSON textareas with
  structured form controls. `modelRef` is now a searchable `<ModelPicker>` that
  shells out to `opencode models --verbose` (like OpenChamber) and displays model
  metadata (cost, context limits, capabilities) in a popup info card. Permissions,
  gated tools, budget overrides, and context sources are now checkboxes, number
  inputs, and multi-select fields with descriptions. A new `AIGatewayService.ListOpenCodeModels`
  RPC drives model discovery; the `ModelDiscoverer` caches results with a 5-minute
  TTL and falls back to a mock list when opencode is absent. Backend proto extended
  with `OpenCodeModel`/`ModelCost`/`ModelLimits`/`ModelCapabilities` messages.
  `<MCPPicker>` stores `MCPConfig[]` objects (id + command) instead of plain IDs;
  discovered from `opencode debug config` (resolved JSON) with well-known MCPs
  (context7, gh_grep, filesystem, github, postgres, slack, playwright, memory)
  merged via `mergeMCPs`. New `UpdateWorkerVersion`/`CreateWorkerVersion` RPCs
  for editing draft versions and forking from published. Worker detail page now
  has inline editing for draft versions and "New version" button for published
  workers (removed spurious edit lock). Build verified: `go build ./...`, `npm run build` pass cleanly.
- **Workflow editor drag fix + palette revamp**: The drag-and-drop bug
  (effectAllowed=`move` mismatched dropEffect=`copy`, no dragenter handler,
  no fall-back for `screenToFlowPosition` returning a NaN/zero) is fixed:
  the palette now sets `effectAllowed="copyMove"`, the wrapper exposes
  `dragenter`/`dragleave` (counter-pattern) so the drop-zone engages
  visually, and the drop handler is defensive against partial state.
  Palette split into four collapsible sections — Workers (published),
  Work items (project-scoped, ready first), Policies (published), and
  Step primitives (decision/approval/parallel/recover) — each with a
  Lucide icon, color-coded accent (sky/emerald/amber/violet/rose/yellow)
  that adapts to the 20-theme system (`dark:bg-*` variants), and a
  hover tooltip with description + example. Drag image is a hidden
  clone of the tile for a custom cursor preview. The drop zone shows
  a "Drop to add step" overlay + border highlight + "Drag a tile from
  the palette to begin." empty-state hint. New `Tooltip` UI primitive
  (Radix) + `EditLockBanner` + `StepNode` + `Palette` + `PropertiesPanel`
  components live under `frontend/src/components/workflow-editor/`. Drag
  a Work Item to create a task step with the work item's title/desc/AC
  pre-populated and `config.work_item_id` linked (zero-backend-cost
  reference); drag a Policy to set `gate_policy_ref` on the new step.
  Step node card now shows the kind icon, name, ref, and a gate badge.
  Properties panel has per-kind fields (worker ULID/version only on
  task steps), policy autocomplete, and a "linked work item" hint when
  the config carries `work_item_id`. Validation: task steps can be
  satisfied by either a worker ref OR a work item reference. New
  keyboard shortcut: `Del`/`Backspace` removes the selected step.
  Build verified: `go build ./...`, `npm run build` pass cleanly.
- **PR B (workflow context propagation)**: Workers now receive a
  composite prompt with the work item (title + desc + AC) + ancestor
  work items (project context) + summaries from prior worker stages
  (upstream context). The lowest-level work item is THE task; the
  rest is added context. The worker is instructed to end its response
  with `ORCHICON WORKER SUMMARY:` followed by a short summary that
  becomes the next stage's input. The marker is best-effort: if
  absent, the entire response is treated as the summary.
  Implementation:
  - `work_items.prompt_context` (jsonb, migration
    `20260713210000_work_items_prompt_context.sql`) carries the
    composite prompt the worker should see. Set by
    `WorkflowReconciler.buildCompositePrompt` before dispatch.
  - `opencode.Adapter` accumulates the worker's text output across
    `text` events and passes it to `OnResult` (extended signature).
  - `TaskReconciler.transitionWorkItemOnResult` extracts the
    ORCHICON WORKER SUMMARY block, persists `_output` + `_summary`
    on `work_items.results`, and propagates `_summary` onto the
    linked workflow step run so downstream stages can compose it as
    upstream context.
  - `db.CountActiveExecutionsForAdapter` excludes `unhealthy` (a
    terminal state) — fix for stuck adapters at capacity.
  - `WorkflowReconciler.upstreamStepSummaries` collects prior TASK
    step runs' `_summary` and includes them in the composite
    prompt.
  - PropertiesPanel shows a "Composite prompt (preview)" block for
    TASK steps with the actual prompt text.
  Verified end-to-end with a 1-stage workflow + real opencode on
  `opencode/deepseek-v4-flash-free`: composite prompt is built with
  task + ancestor; worker's output is parsed for the marker and the
  summary is stored on `work_items.results._summary`; the summary
  is propagated to `workflow_step_runs.results._summary`.
- **PR C (recovery as typed work items)**: Recovery is now a typed
  work item. The proto `WorkItemKind` gains 4 recovery enum values
  (`RECOVERY_STOP`, `RECOVERY_SUMMARIZE_RESTART`,
  `RECOVERY_HUMAN_ESCALATION`, `RECOVERY_RETRY_N`). The
  `recovery_executions` table gains a `strategy` column (migration
  `20260715000000_recovery_strategy.sql`); the `RecoveryReconciler`
  reads `strategy` at the top of its loop and routes per strategy
  (stop → mark failed/cancelled; human_escalation → block at L3;
  retry_n → requeue immediately; summarize_restart → 6-step default
  flow). `strategyForWorkItem(workItem.Kind)` maps the new recovery
  kinds to their strategies; default kinds still map to
  summarize_restart. The user-facing palette UI for the new recovery
  kinds is PR D (no regression for existing flows).
- **PR D (workflow editor polish — delete, Start/Stop, Recovery palette)**:
  The workflow editor gets four new affordances that were flagged as
  missing in the original drag-fix PR: (1) hover-only × on each step
  node's top-right corner; (2) hover-only × at the midpoint of each
  edge via a custom `DeletableEdge` React Flow edge type and an
  `EdgeLabelRenderer`; (3) an inline `Stop` button on the editor
  header — shown only when the most recent run is in pending or
  running state — using the existing `useAbortWorkflow` mutation;
  (4) a new `Recovery` palette section with 4 tiles (Summarize +
  restart / Stop / Human escalation / Retry N) that drag RECOVER
  step primitives with a `strategy` field in config, and a
  `RecoveryStrategyField` in the properties panel for editing the
  strategy on a selected recover step. The `WorkflowReconciler` reads
  `config.strategy` on RECOVER steps and logs the resolved value so
  operators can see what would have run on a real failure. Build
  verified: `go build`, `npm run build`, `make ci` all pass; smoke
  test (`/tmp/opencode/e2e_prd.sh`) confirms the strategy field
  round-trips through the workflow API and the reconciler logs it.
- **Background server + log management + force-clean installer**: `orchicon
  dev start` now forks the control plane as a background child process
  (separate process group on Unix so Ctrl-C doesn't kill it), writes
  structured logs to `.dev/logs/orchicon.log`, waits for `/healthz`, then
  automatically tails the log file (Ctrl-C stops the tail only). The server
  keeps running in the background until `orchicon dev stop` is called, which
  sends SIGTERM (with 15s grace period then SIGKILL) and runs `docker compose
  down`. New `orchicon dev logs` / `orchicon logs` subcommand tails the log
  file on demand. Startup and shutdown messages are detailed with clear
  progress indicators. The installers (`scripts/install.sh` + `install.ps1`)
  gained `--force-clean` / `-ForceClean` flags that nuke all Docker volumes
  (`docker compose down -v --remove-orphans`), remove `data/`, `.dev/`, and
  `bin/` directories, then install the latest binary. Build verified: `go
  build ./cmd/orchicon` passes cleanly.
