# Orchicon

AI orchestration and operations platform that coordinates autonomous AI
work as reliable, observable, recoverable, and manageable systems.

Orchicon separates **orchestration** from **execution**: it manages
projects, workers, scheduling, policies, telemetry, recovery, and
governance, while pluggable runtimes execute the work.

> Orchicon orchestrates. Runtimes execute.

## Documentation

All architecture and design documents live in [`docs/`](./docs):

| # | Document | Concern |
|---|---|---|
| 00 | [Architecture Design Document](./docs/00_Architecture_Design_Document.md) | Original design brief — vision, principles, core concepts |
| 01 | [Architecture Vision](./docs/01_Architecture_Vision.md) | Tech direction, system topology, design principles |
| 02 | [Domain Model](./docs/02_Domain_Model.md) | Projects, Workers, Tasks, Workflows, Policies, Recovery |
| 03 | [Scheduler & Runtime Design](./docs/03_Scheduler_and_Runtime_Design.md) | Reconciler architecture, dispatch flow, health monitoring |
| 04 | [Runtime Adapter SDK](./docs/04_Runtime_Adapter_SDK.md) | gRPC adapter contract, OpenCode first |
| 05 | [Worker Specification](./docs/05_Worker_Specification.md) | Worker entity, permissions, budgets, versioning |
| 06 | [Recovery Workflow Engine](./docs/06_Recovery_Workflow_Engine.md) | Triggers, default workflow, escalation, continuation plans |
| 07 | [API Specification](./docs/07_API_Specification.md) | REST/gRPC/WebSocket via Protobuf + Connect |
| 08 | [Event Bus & Telemetry Model](./docs/08_Event_Bus_and_Telemetry_Model.md) | NATS JetStream, OTel, SigNoz/ClickHouse |
| 09 | [Database Schema](./docs/09_Database_Schema.md) | PostgreSQL, outbox, RLS, Atlas migrations |
| 10 | [Frontend Architecture](./docs/10_Frontend_Architecture.md) | React/Vite, Connect-ES, visual workflow editor |

The original design brief: [`00_Architecture_Design_Document.md`](./docs/00_Architecture_Design_Document.md)

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

**v0.1.98** — Telemetry fixes, OTel log export, workflow editor enhancements.
Telemetry queries ClickHouse directly bypassing SigNoz API incompatibility.
OTel log exporter sends adapter stderr to ClickHouse as structured logs.
Workflow editor: uniform node sizing, Clone button, Save Draft fixed after
creating new versions, Delete version button, YAML code view toggle with
`connections` format (from_<pos>/to_<pos>). Cost Explorer shows names via
per-task lookups. Credits tab with provider/model spend. /signoz reverse
proxy. composeDown force-removes orphaned containers on start/stop.

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

Generated code is committed (docs/10 §3.1).

### Layout

| Path | Concern |
|---|---|
| `cmd/orchicon/` | Control-plane binary entry point + `dev` subcommand |
| `internal/` | api, auth, config, db, domain, eventbus, outbox, reconciler, server, telemetry, migrate, middleware, rbac, tenant, blobstore, webhook, version |
| `assets.go` | go:embed directives for compose, migrations, frontend |
| `proto/` | Protobuf schema (`orchicon.api.v1`, `orchicon.adapter.v1`) |
| `api/gen/` | Generated Go code |
| `db/` | Atlas declarative schema + versioned migrations |
| `deploy/compose/` | Local dev Docker Compose stack |
| `frontend/` | Vite + React + Connect-ES + TanStack Router + shadcn/ui |
| `scripts/` | CI gates (RLS check) |

### CI gate

```bash
make ci          # buf lint + codegen + go vet/test + RLS gate
```

The RLS gate (docs/09 §8.5) fails if any `tenant_id`-bearing table
lacks the `tenant_isolation` policy.

## License

Copyright © 2026 beardedparrott. All rights reserved.

This software is provided free of charge for personal and non-commercial
use. You may use, copy, and modify it for your own non-commercial
purposes. Redistribution, sublicensing, or integration into commercial
products that generate revenue requires explicit written permission from
the owner. See the [LICENSE](./LICENSE) file for the full terms.
