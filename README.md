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

The original design brief: [`Orchicon_Architecture_Design_Document_v0.1.md`](./Orchicon_Architecture_Design_Document_v0.1.md)

## Technology Stack

- **Control plane**: Go (single binary, k8s-style reconcilers)
- **API**: Protobuf + Connect (gRPC + REST + streaming from one schema)
- **Database**: PostgreSQL 16 with RLS + transactional outbox
- **Event bus**: NATS JetStream
- **Telemetry**: OpenTelemetry → SigNoz (ClickHouse) — fully separated infra
- **Policy**: Rego (Open Policy Agent)
- **Runtime adapters**: gRPC sidecars (OpenCode first)
- **Frontend**: TypeScript + React + Vite + Connect-ES

## Status

**v0.1 — scaffolding.** Architecture documents are direction-level.
Phase 1 (Foundation) is landed: Go module, Protobuf schema + Connect
codegen, Atlas migrations with RLS, Docker Compose dev stack, and the
Vite+React frontend shell.

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

```bash
# Install a specific version
curl -fsSL https://orchicon.dev/install | bash -s -- --version v0.2.0

# Uninstall
curl -fsSL https://orchicon.dev/install | bash -s -- --uninstall
```

After installation, verify with `orchicon version` and start the dev
environment with `orchicon dev start` — the binary embeds the Docker
Compose stack, migrations, and frontend bundle, so it's the complete
one-command experience (requires Docker).

> **Note:** Pre-built binaries are published to [GitHub
> Releases](https://github.com/beardedparrott/Orchicon/releases). If no
> releases exist yet (pre-v1), build from source instead:

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
| `internal/` | api, config, db, domain, eventbus, outbox, reconciler, server, telemetry, migrate, middleware, tenant, blobstore, version |
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

TBD
