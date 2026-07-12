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
| 2 | Projects slice | not started | Project CRUD (full stack) + frontend project list and detail views |
| 3 | Realtime + infrastructure | not started | Outbox relay, reconciler framework, OTel, frontend streaming (`useStream` hook) |
| 4 | Workers + WorkItems | not started | Worker versioning, WorkItem hierarchy, dependencies + frontend catalog, tree/board, dependency graph |
| 5 | Scheduling + adapters | not started | TaskReconciler, dispatch, OpenCode adapter + frontend execution live view |
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
- **Phase 2 entry point**: the `ProjectService` proto and Connect-ES
  client already exist; the next step is the Go handler + data-access
  layer in `internal/` wiring the generated `api/gen/go` service.
