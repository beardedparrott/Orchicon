# Orchicon

AI orchestration and operations platform that coordinates autonomous AI
work as reliable, observable, recoverable, and manageable systems.

Orchicon separates **orchestration** from **execution**: it manages
projects, workers, scheduling, policies, telemetry, recovery, and
governance, while pluggable runtimes execute the work.

> Orchicon orchestrates. Runtimes execute.

## Documentation

The comprehensive project documentation lives in
[`DOCUMENTATION.md`](./DOCUMENTATION.md) at the project root. It
covers architecture, project structure, installation, development,
deployment, troubleshooting, and every subsystem.

## Technology Stack

- **Control plane**: Go (single binary, k8s-style reconcilers)
- **API**: Protobuf + Connect (gRPC + REST + streaming from one schema)
- **Database**: PostgreSQL 16 with RLS + transactional outbox
- **Event bus**: NATS JetStream
- **Telemetry**: OpenTelemetry → SigNoz (ClickHouse) — fully separated infra
- **Policy**: Rego (Open Policy Agent)
- **Runtime adapters**: gRPC sidecars (OpenCode first)
- **Frontend**: TypeScript + React + Vite + Connect-ES

## Last Release Changes

### v0.1.151 (2026-07-28)

| Type | Change |
|---|---|
| Feature | Ask Orchicon conversational agent — 11 RPCs, server-streaming ChatStream, ToolRegistry with 25+ tools, MCP server, frontend /ask-orchicon route |
| Feature | MCP auto-injection — user's opencode config MCP servers merged into every worker execution and Ask Orchicon chat |
| Feature | Dynamic adapter capabilities — reflects MCP servers and model providers from user's opencode config |
| Feature | Seed data rework — canned workers moved from SQL migrations to Go code (idempotent startup seeding) |
| Chore | AGENTS.md cleanup — removed destructive E2E cleanup checklist, added database backup protocol |
| Bug fix | Worker-backed approval dispatch stuck in approval_pending — step.Ref now used as worker reference |
| Feature | RetryStepRun RPC + "Retry step" UI button to re-dispatch stuck step runs |

### v0.1.150 (2026-07-27)

| Type | Change |
|---|---|
| Feature | Settings page replaces Preferences — adds Defaults sections (default models, stall params) |
| Feature | Tenant-level settings stored in DB (tenant_settings table) with GetSettings/UpdateSettings RPCs |
| Feature | Default worker model in settings — dispatch fails if both worker model_ref and default are empty |
| Feature | Default Ask Orchicon model setting (placeholder for forthcoming feature) |
| Feature | Recovery stall parameters configurable in Settings UI (DB-backed, env-var override) |
| Feature | Cost Explorer "By Workflow" tab — cost breakdown per workflow run with per-step drill-down |
| Feature | Adapter reads stall thresholds from tenant settings at dispatch time (env fallback) |

### v0.1.149 (2026-07-27)

| Type | Change |
|---|---|
| Feature | Human-in-the-loop APPROVAL step kind with approve/reject UI and loop-back on rejection |
| Feature | Worker-backed approval: AI Approver worker evaluates and decides approve/reject automatically |
| Feature | Native loop-back in approval steps (no separate loop_decision node needed) |
| Feature | DevOps Engineer worker (GitOps, repo setup, PR/merge after approval) |
| Feature | AI Approver worker (evaluates context, outputs approve/reject) |
| Feature | Principal Software Architect worker (seeded for fresh installs) |
| Feature | Worker identity (Role, Skills, Behavior, AGENTS.md) included in composite prompt |
| Feature | Workflow-aware step numbering with topological sort (step N of M) |
| Feature | Iteration context and execution history timeline in worker prompts |
| Feature | Bulk select/approve/reject on Approvals page |
| Feature | Editable worker purpose field (UpdateWorker RPC) |
| Feature | Auto-start workflow on save (create + update), mutually exclusive with scheduled time |
| Feature | Custom lock button that disables pan, select, and drag |
| Feature | MiniMap positioned top-right, resizable, smaller default |
| Bug fix | Workers know who they are (Role/Skills/Behavior/AGENTS.md now in prompt) |
| Bug fix | Stale edit locks cleared on server restart (DELETE FROM edit_locks on startup) |
| Bug fix | `_issues:` no longer auto-preserved across iterations — prevents false failure signals |
| Bug fix | PR Reviewer and QA Engineer focus on real bugs, not nitpicks |
| Bug fix | Clone work item preserves kind and parentId |
| Bug fix | Worker version cache invalidation — saved draft changes reflect immediately |
| Bug fix | Approval list excludes skipped/blocked/pending step runs |
| Bug fix | Approved vs rejected properly distinguished via _decision field |
| Chore | AGENTS.md: DOCUMENTATION.md sync rule + UI consistency rule |
| Chore | Migrations: publish AI Approver, seed Architect, unify decision format |
| Chore | Install script runs migrations (graceful fallback) |

## Installation

### One-line install (Linux / macOS)

```bash
curl -fsSL https://orchicon.dev/install | bash
```

### Windows (PowerShell)

```powershell
irm https://orchicon.dev/install.ps1 | iex
```

### Options

| Flag | Description |
|---|---|
| `--version <tag>` | Install a specific version (e.g. `v0.2.0`). Default: latest. |
| `--install-dir <dir>` | Installation directory (default: `~/.local/bin`). |
| `--uninstall` | Remove Orchicon from the install directory. |
| `--dry-run` | Print what would happen without making changes. |
| `--clean` | Stop dev containers, remove old binary, then install latest. All user data preserved. |
| `--force-clean` / `--nuke` | Wipe everything: destroy Docker volumes, remove blob store data and runtime state, then install latest. **All data lost.** |

```bash
# Install a specific version
curl -fsSL https://orchicon.dev/install | bash -s -- --version v0.2.0

# Uninstall
curl -fsSL https://orchicon.dev/install | bash -s -- --uninstall

# Clean upgrade (preserves data)
curl -fsSL https://orchicon.dev/install | bash -s -- --clean

# Force clean and reinstall (destroys all data)
curl -fsSL https://orchicon.dev/install | bash -s -- --force-clean
```

After installation, verify with `orchicon version` and start the dev
environment with `orchicon dev start`.

> **Note:** Pre-built binaries are published to [GitHub
> Releases](https://github.com/beardedparrott/Orchicon/releases). If no
> releases exist yet (pre-v1), build from source instead:

### What gets installed

| Path | Contents |
|---|---|
| `<install-dir>/orchicon` | The `orchicon` binary (control plane + embedded frontend) |
| `~/.local/share/orchicon/` | Runtime state, PID files, logs (`.dev/`), blob store (`data/`) |

### Commands

| Command | Description |
|---|---|
| `orchicon dev start` | Start full dev stack: Docker Compose services, migrations, control plane, frontend |
| `orchicon dev stop` | Stop everything (SIGTERM + Docker Compose down) |
| `orchicon dev status` | Show status of all components + endpoint checks |
| `orchicon dev logs` | Tail control-plane and frontend logs |
| `orchicon version` | Print the installed version |

```bash
git clone https://github.com/beardedparrott/Orchicon.git
cd Orchicon
make build          # → bin/orchicon
make dev-start      # full dev environment
```

## Development

The control plane is Go; the frontend is TypeScript + Vite. All common
tasks are in the `Makefile` (`make help`).

### Prerequisites

- Go 1.26+
- Node 22+
- Docker + Docker Compose
- [`buf`](https://buf.build) and [`atlas`](https://atlasgo.io) — install
  with `make tools`

### Quick start

```bash
make up           # start Postgres, NATS, SigNoz, OTel collector
make migrate      # apply Atlas migrations (tenants, identities, projects + RLS)
make run          # run the control plane on :8080
make fe-install   # install frontend deps (first time only)
make fe-dev       # Vite dev server on :5173 (proxies API to :8080)
```

### Authentication

The control plane authenticates every RPC. In local mode
(`ORCHICON_OIDC_ISSUER=local`) a built-in dev identity provider mints
short-lived access tokens + refresh tokens with no external IdP — the
full auth flow is verifiable locally. Production sets a real OIDC
issuer (`ORCHICON_MODE=production` enforces this on boot). The frontend
login page (`/login`) offers both the dev IdP and OIDC SSO. See
`.env.example` for the auth config variables.

### Codegen

The Protobuf schema (`proto/`) is the single source of truth. One
schema generates the Go (connect-go) and TypeScript (Connect-ES)
clients:

```bash
make gen          # buf generate → api/gen/go + frontend/src/api/gen
```

Generated code is committed (see DOCUMENTATION.md §Code Generation).

### Layout

| Path | Concern |
|---|---|---|
| `DOCUMENTATION.md` | Comprehensive project documentation |
| `cmd/orchicon/` | Control-plane binary entry point + `dev` subcommand |
| `internal/` | api, auth, config, db, domain, eventbus, outbox, reconciler, server, telemetry, migrate, middleware, rbac, tenant, blobstore, webhook, version |
| `assets.go` | go:embed directives for compose, migrations, frontend |
| `proto/` | Protobuf schema (`orchicon.api.v1`, `orchicon.adapter.v1`) |
| `api/gen/` | Generated Go code |
| `db/` | Atlas declarative schema + versioned migrations |
| `deploy/compose/` | Local dev Docker Compose stack |
| `frontend/` | Vite + React + Connect-ES + TanStack Router + shadcn/ui |
| `site/` | Static landing page (`orchicon.dev`) |
| `scripts/` | Installers, CI gates, dev controller |

### CI gate

```bash
make ci          # buf lint + codegen + go vet/test + RLS gate
```

The RLS gate (see DOCUMENTATION.md §Key Architecture Invariants) fails if any `tenant_id`-bearing table
lacks the `tenant_isolation` policy.

## License

Copyright © 2026 beardedparrott. All rights reserved.

This software is provided free of charge for personal and non-commercial
use. You may use, copy, and modify it for your own non-commercial
purposes. Redistribution, sublicensing, or integration into commercial
products that generate revenue requires explicit written permission from
the owner. See the [LICENSE](./LICENSE) file for the full terms.
