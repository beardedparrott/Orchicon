# Orchicon — Comprehensive Documentation

> **Orchicon** is an AI orchestration and operations platform. It coordinates autonomous AI work as reliable, observable, recoverable, and manageable systems by separating **orchestration** from **execution**. The control plane manages projects, workers, scheduling, policies, telemetry, recovery, and governance, while pluggable runtimes execute the actual work.

---

## Table of Contents

1. [Project Overview](#project-overview)
2. [Technologies Used](#technologies-used)
3. [Overall Architecture](#overall-architecture)
4. [Project Structure](#project-structure)
5. [Installation Guide](#installation-guide)
6. [User Guide](#user-guide)
7. [Development Guide](#development-guide)
8. [Deployment](#deployment)
9. [Environment Variables Reference](#environment-variables-reference)
10. [Troubleshooting](#troubleshooting)
11. [Contributing](#contributing)
12. [License](#license)

---

## Project Overview

Orchicon is an open-core platform for orchestrating autonomous AI agents. It provides a **control plane** (single Go binary) that manages the full lifecycle of AI work: defining workers (agent personas with permissions and budgets), organizing work into projects and work-item DAGs, scheduling tasks to available runtime adapters, monitoring execution with OpenTelemetry, enforcing governance policies via OPA/Rego, and recovering from failures with a built-in workflow engine.

The frontend is a TypeScript/React SPA with a visual React Flow workflow editor, real-time execution streaming, and an embedded SigNoz telemetry dashboard. The entire local development stack — PostgreSQL, NATS JetStream, OpenTelemetry, SigNoz, ClickHouse — starts with a single command.

> **Orchicon orchestrates. Runtimes execute.**

---

## Technologies Used

### Backend (Control Plane)

| Technology | Version | Purpose |
|---|---|---|
| Go | 1.26.4 | Control plane language |
| connectrpc.com/connect | v1.20.0 | RPC framework (gRPC + REST + streaming from one Protobuf schema) |
| github.com/jackc/pgx/v5 | latest | PostgreSQL driver (pool, transactions, prepared statements) |
| github.com/nats-io/nats.go | latest | NATS JetStream client (event bus) |
| github.com/open-policy-agent/opa | latest | Open Policy Agent v1 (Rego policy engine) |
| go.opentelemetry.io/otel | latest | OpenTelemetry SDK (traces, metrics, logs) |
| go.opentelemetry.io/otel/exporters/otlp/... | latest | OTLP gRPC exporters |
| github.com/coreos/go-oidc/v3 | latest | OIDC authentication |
| github.com/oklog/ulid/v2 | latest | ULID generation for IDs |
| github.com/aws/aws-sdk-go-v2 | latest | AWS SDK v2 (S3 blob store) |
| github.com/lestrrat-go/jwx/v3 | latest | JWT signing/verification |
| google.golang.org/protobuf | latest | Protobuf runtime |
| google.golang.org/grpc | latest | gRPC (used for OTel exporters) |

### Frontend (SPA)

| Technology | Version | Purpose |
|---|---|---|
| TypeScript | 5.7.3 | Frontend language |
| React | 18.3.1 | UI framework |
| Vite | 6.1.0 | Build tool / dev server |
| @connectrpc/connect-web | 1.4.0 | Connect-ES web transport (gRPC-Web) |
| @bufbuild/protobuf | 1.10.0 | Protobuf runtime (TypeScript) |
| @tanstack/react-query | 5.66.0 | Server state management |
| @tanstack/react-router | 1.114.0 | File-based type-safe routing |
| reactflow | 11.11.4 | DAG/node editor for workflow canvas |
| tailwindcss | 3.4.17 | Utility-first CSS framework |
| zustand | 5.0.3 | Lightweight UI-only state management |
| zod | 4.4.3 | Schema validation |
| react-markdown + remark-gfm | latest | Markdown rendering |
| lucide-react | 0.475.0 | Icon library |
| shadcn/ui (Radix primitives) | latest | UI component primitives |
| yaml | 2.9.0 | YAML serialization for workflows |

### Infrastructure & Services

| Service | Technology | Purpose |
|---|---|---|
| Database | PostgreSQL 16 (Alpine) | Primary data store |
| Message Broker | NATS 2.10 (JetStream) | Event bus, at-least-once delivery |
| Observability | SigNoz (community) | Traces, metrics, logs dashboard |
| Time-Series DB | ClickHouse 25.8 | SigNoz backend (embedded Keeper, no ZooKeeper) |
| OTel Collector | SigNoz OTel Collector | Pipeline for traces/metrics/logs |
| Object Storage | Local filesystem or S3 | Blob store abstraction |
| Policy Engine | OPA v1 (Rego) | Governance policy evaluation |
| Runtime Adapter | OpenCode CLI | Default AI agent runtime (pluggable via gRPC) |

---

## Overall Architecture

### System Topology

```mermaid
graph TB
    subgraph "Control Plane (Go Binary)"
        HTTP[HTTP Server :8080]
        GRPC[gRPC Server :9090]
        Connect[Connect-ES Handlers]
        Auth[Auth Middleware<br/>OIDC / API Keys / RBAC]
        Tenant[Tenant Resolution]
        Reconcilers[Reconciler Manager<br/>Task / Workflow / Recovery / ScheduledRun]
        OutboxRelay[Outbox Relay]
        WebhookDispatch[Webhook Dispatcher]
        Policy[OPA Policy Engine]
        RecoveryEngine[Recovery Engine]
        AdapterBridge[OpenCode Adapter Bridge]
        AIGateway[AI Gateway<br/>Model / MCP Discovery]
        Telemetry[OpenTelemetry Setup<br/>Tracer / Meter / Logger]
        BlobStore[BlobStore<br/>Local / S3]
    end

    subgraph "Data Layer"
        PG[(PostgreSQL 16<br/>+ RLS)]
        NATS[(NATS JetStream)]
        ClickHouse[(ClickHouse)]
    end

    subgraph "Observability Stack"
        OTel[OTel Collector]
        SigNoz[SigNoz UI]
    end

    subgraph "Runtime"
        OpenCode[OpenCode CLI]
        FutureRuntime[Future Runtimes<br/>gRPC Sidecar]
    end

    subgraph "Frontend (Browser)"
        SPA[React SPA :5173<br/>Vite / TanStack Router]
        ReactFlow[React Flow<br/>Workflow Editor]
        SigNozIFrame[Embedded SigNoz]
    end

    HTTP --> Connect
    GRPC --> Connect
    Connect --> Auth
    Auth --> Tenant
    Tenant --> Reconcilers
    Reconcilers --> PG
    Reconcilers --> OutboxRelay
    Reconcilers --> AdapterBridge
    OutboxRelay --> NATS
    NATS --> WebhookDispatch
    WebhookDispatch --> HTTP
    AdapterBridge --> OpenCode
    AdapterBridge -.-> FutureRuntime
    AIGateway --> OpenCode
    Connect --> Policy
    Connect --> RecoveryEngine
    Telemetry --> OTel
    OTel --> ClickHouse
    OTel --> SigNoz
    SigNoz --> ClickHouse
    SigNoz -.-> SigNozIFrame
    SPA -.-> HTTP
    ReactFlow --> SPA
```

### Data Flow

```mermaid
sequenceDiagram
    participant U as User / UI
    participant API as Connect API
    participant DB as PostgreSQL
    participant O as Outbox
    participant N as NATS
    participant R as Reconciler
    participant A as Adapter Bridge
    participant RT as Runtime (OpenCode)

    U->>API: CreateWorkItem
    API->>DB: INSERT work_item (tx)
    API->>DB: INSERT outbox event (tx)
    DB-->>API: commit
    API-->>U: 200 OK

    O->>DB: Poll unpublished outbox rows
    DB-->>O: New event
    O->>N: Publish to JetStream
    N-->>O: Ack

    Note over R: TaskReconciler scan pass
    R->>DB: SELECT ready work_items
    DB-->>R: Ready tasks
    R->>R: Dependency resolution
    R->>R: Worker selection (health, LRU)
    R->>A: Dispatch execution
    A->>RT: Start subprocess (opencode CLI)
    RT-->>A: JSON telemetry events (stdout)
    A->>DB: INSERT execution records
    A->>N: Publish execution events
    RT-->>A: Exit
    A->>R: Execution callbacks (complete/fail)
    R->>DB: UPDATE work_item status
    R->>DB: INSERT outbox event
```

### Key Architectural Patterns

1. **Kubernetes-style Reconcilers** — Four reconcilers (Task, Workflow, Recovery, ScheduledRun) run in a shared manager with per-kind PostgreSQL advisory locks for leader election. Each has a work queue with exponential backoff and a scan pass for discovering work.

2. **Transactional Outbox** — Every mutation writes an outbox row in the same database transaction as the state change. A background relay polls unpublished rows every 500ms and publishes to NATS JetStream for at-least-once delivery.

3. **Single Binary** — The Go binary embeds Docker Compose files, SQL migrations, and the built frontend SPA via `go:embed`. No external dependencies at runtime beyond Docker for the infrastructure services.

4. **Non-blocking OTel** — The OpenTelemetry pipeline uses `grpc.NewClient` (non-blocking dial), so the control plane boots in <2 seconds even when the OTel collector is not yet healthy.

5. **Connect-ES** — Single Protobuf schema generates both Go server and TypeScript client code. Supports unary RPC, server-streaming, and client-streaming over the same interface.

6. **RLS-backed Tenant Isolation** — Every tenant-scoped table has a PostgreSQL Row-Level Security policy as a backstop. The data-access layer also injects `app.tenant_id` via session variables.

7. **Adapter Bridge Pattern** — Runtimes are pluggable gRPC sidecars. The built-in adapter wraps the OpenCode CLI as a subprocess, parsing its JSON telemetry output. Future runtimes implement the `orchicon.adapter.v1` gRPC contract.

### Domain Model

```mermaid
erDiagram
    Tenant ||--o{ Project : has
    Project ||--o{ WorkItem : contains
    Project ||--o{ Worker : defines
    Project ||--o{ Workflow : orchestrates
    WorkItem ||--o{ WorkItem : depends_on
    WorkItem ||--o{ WorkerExecution : triggers
    Worker ||--o{ WorkerExecution : assigned_to
    Workflow ||--o{ WorkflowVersion : versions
    WorkflowVersion ||--o{ WorkflowRun : runs
    WorkflowRun ||--o{ WorkflowStepRun : steps
    WorkerExecution ||--o{ WorkflowStepRun : produces
    WorkerExecution ||--o{ RecoveryExecution : triggers_on_failure
    Project ||--o{ Policy : governed_by
    Worker ||--o{ Policy : assessed_against

    Tenant {
        string id ULID
        string name
    }
    Project {
        string id ULID
        string tenant_id
        string name
        string slug
        string status
        json goals
    }
    WorkItem {
        string id ULID
        string tenant_id
        string project_id
        string kind "Epic|Feature|Task|Subtask"
        string status "draft|ready|assigned|running|done|failed|cancelled"
        string assigned_worker_ref
    }
    Worker {
        string id ULID
        string tenant_id
        string project_id
        string status "draft|published|deprecated|retired"
        json model_ref
        json budget_overrides
    }
    WorkerExecution {
        string id ULID
        string tenant_id
        string work_item_id
        string worker_id
        string status "pending|dispatching|running|success|failure|cancelled"
        json prompt_context
    }
    Workflow {
        string id ULID
        string tenant_id
        string project_id
        string type "one_shot|template"
    }
    WorkflowRun {
        string id ULID
        string workflow_version_id
        string status "pending|running|completed|failed"
        string work_item_id
    }
    Policy {
        string id ULID
        string tenant_id
        string project_id
        string rego_module
    }
```

---

## Project Structure

```
Orchicon/
├── AGENTS.md                    # AI agent entry point & development guidelines
├── assets.go                    # go:embed: Docker Compose, migrations, frontend
├── buf.gen.yaml                 # Buf codegen config (Go + TypeScript)
├── buf.yaml                     # Buf lint config
├── CLOUDFLARE_SETUP.md          # Cloudflare Pages one-time setup guide
├── DOCUMENTATION.md             # ← This file: comprehensive docs
├── LICENSE                      # Custom license (non-commercial)
├── Makefile                     # All targets: build, test, gen, up, ci, dev-*
├── opencode.jsonc               # Opencode tool configuration
├── README.md                    # Project introduction & quick start
├── UPDATES.md                   # Per-PR change tracking
├── wrangler.toml                # Cloudflare Pages project config
│
├── cmd/
│   └── orchicon/                # Go binary entry point
│       ├── main.go              # Subcommand dispatch (run, dev, start, stop, etc.)
│       ├── dev.go               # `orchicon dev` subcommand implementation
│       ├── dev_procattr_unix.go # Unix process attributes for background fork
│       ├── dev_procattr_windows.go # Windows process attributes
│       ├── orphans_unix.go      # Unix orphan process cleanup
│       └── orphans_windows.go   # Windows orphan process cleanup
│
├── internal/
│   ├── adapter/                 # RuntimeAdapterService (list adapters, capabilities)
│   ├── aigateway/               # AI Gateway: model/MCP discovery, usage recording
│   ├── api/                     # Connect handler mounting, SigNoz reverse proxy
│   ├── auth/                    # OIDC, API keys, JWT tokens, identity resolution
│   ├── blobstore/               # Blob abstraction: local filesystem + S3
│   ├── config/                  # Environment-driven configuration
│   ├── db/                      # Data-access layer (pgx + tenant scoping)
│   ├── domain/                  # Core domain types, constants, lifecycle states
│   ├── eventbus/                # NATS JetStream publisher + subscriber
│   ├── execution/               # ExecutionService handler
│   ├── middleware/              # Auth + tenant resolution middleware
│   ├── migrate/                 # In-binary SQL migration runner
│   ├── opencode/                # OpenCode CLI adapter bridge + stall detection
│   ├── outbox/                  # Outbox event types + background relay
│   ├── policy/                  # OPA/Rego policy engine + PolicyService
│   ├── project/                 # ProjectService + validation
│   ├── rbac/                    # RBAC Connect interceptor
│   ├── reconciler/              # Reconciler framework (work queue, leader election)
│   ├── recovery/                # Recovery engine + RecoveryService
│   ├── scheduler/               # TaskReconciler, WorkflowReconciler, ScheduledRunReconciler
│   ├── server/                  # Composition root (wires all dependencies)
│   ├── telemetry/               # OTel setup, SigNoz client, telemetry service
│   ├── tenant/                  # Tenant context plumbing
│   ├── version/                 # Build-time version metadata
│   ├── webhook/                 # Webhook dispatcher + WebhookService
│   ├── worker/                  # WorkerService + validation
│   ├── workflow/                # WorkflowService + validation
│   └── workitem/                # WorkItemService + validation
│
├── proto/
│   └── orchicon/
│       ├── adapter/v1/          # Adapter gRPC sidecar contract
│       │   └── adapter.proto
│       └── api/v1/              # Public API protobuf schema (12 services)
│           ├── project{,_service}.proto
│           ├── worker{,_service}.proto
│           ├── work_item{,_service}.proto
│           ├── workflow{,_service}.proto
│           ├── execution{,_service}.proto
│           ├── policy{,_service}.proto
│           ├── recovery{,_service}.proto
│           ├── telemetry{,_service}.proto
│           ├── auth{,_service}.proto
│           ├── ai_gateway{,_service}.proto
│           ├── adapter{,_service}.proto
│           └── webhook_service.proto
│
├── api/gen/go/                  # Generated Go code from protobuf
│
├── db/
│   ├── atlas.hcl                # Atlas migration config
│   ├── schema.hcl               # Declarative schema source of truth (21 tables)
│   └── migrations/              # 30 versioned SQL migration files (forward-only)
│       ├── 20260712192105_initial_schema.sql
│       └── ... (30 total)
│
├── deploy/
│   └── compose/
│       ├── docker-compose.yml           # 6 services: PG, NATS, ClickHouse, SigNoz, OTel
│       ├── clickhouse-cluster.xml        # ClickHouse Keeper config (no ZooKeeper)
│       ├── init-postgres.sql             # Postgres init script
│       ├── otel-collector-config.yaml    # OTel pipeline config
│       └── signoz-config.yaml            # SigNoz query-service config
│
├── frontend/
│   ├── index.html                # Vite entry HTML
│   ├── package.json              # All npm dependencies
│   ├── vite.config.ts            # Vite config (proxy, plugins, aliases)
│   ├── tailwind.config.js        # Tailwind CSS config
│   ├── tsconfig.json             # TypeScript config
│   └── src/
│       ├── main.tsx              # React entry: providers + router
│       ├── router.tsx            # TanStack Router setup
│       ├── routeTree.gen.ts      # Auto-generated route tree
│       ├── index.css             # Global styles + 15 themes (Tailwind + CSS vars)
│       ├── auth/                 # AuthProvider, session management
│       ├── api/                  # Connect-ES clients, hooks, streaming
│       │   ├── clients.ts        # 12 generated service clients
│       │   ├── useStream.ts      # Generic server-stream hook
│       │   ├── projects.ts       # Project hooks (useListProjects, etc.)
│       │   └── ...               # One module per service
│       ├── components/           # Shared components
│       │   ├── app-shell.tsx     # Sidebar + topbar + content layout
│       │   ├── theme-provider.tsx # Theme application
│       │   ├── markdown.tsx      # Reusable Markdown renderer
│       │   ├── workflow-editor/  # React Flow editor components
│       │   │   ├── StepNode.tsx
│       │   │   ├── DeletableEdge.tsx
│       │   │   ├── Palette.tsx
│       │   │   ├── PropertiesPanel.tsx
│       │   │   ├── CodeView.tsx
│       │   │   ├── EditLockBanner.tsx
│       │   │   ├── stepKinds.ts
│       │   │   ├── canvas.ts
│       │   │   └── workflowYaml.ts
│       │   ├── executions/       # Execution detail components
│       │   └── ui/               # shadcn/ui primitives
│       └── routes/               # File-based TanStack Router pages
│           ├── __root.tsx        # Root layout
│           ├── index.tsx         # Dashboard (/)
│           ├── login.tsx         # Login page
│           ├── projects{,_.new,_.$id}.tsx
│           ├── work-items{,_.new,_.$id,_.graph}.tsx
│           ├── workers{,_.new,_.$id}.tsx
│           ├── workflows{,_.new,_.$id,_.$id_.runs.$runId}.tsx
│           ├── policies{,_.new,_.$id}.tsx
│           ├── recovery{,_.$id}.tsx
│           ├── executions{,_.$id}.tsx
│           ├── telemetry.tsx
│           ├── adapters.tsx
│           ├── webhooks.tsx
│           ├── settings.tsx
│           ├── settings.ts
│           └── admin.tsx
│
├── scripts/
│   ├── install.sh               # Linux/macOS one-liner installer
│   ├── install.ps1              # Windows PowerShell installer
│   ├── install-local.sh         # Build & install from local source
│   ├── dev.sh                   # Dev environment controller (start/stop/status/logs)
│   ├── build-site.sh            # Cloudflare Pages build step
│   ├── check-rls.sh             # RLS CI gate (tenant isolation verification)
│   └── hf-latest-models.sh      # Hugging Face model fetcher utility
│
├── site/                        # Landing page (orchicon.dev)
│   ├── index.html               # Static HTML landing page
│   ├── style.css                # Landing page styles
│   ├── install                  # Gitignored build artifact (copy of scripts/install.sh)
│   └── install.ps1              # Gitignored build artifact
│
└── .github/
    ├── CODEOWNERS               # @beardedparrott owns everything
    └── workflows/
        ├── auto-release.yml     # Auto-bumps version when release-labeled PR merges
        └── release.yml          # Builds binaries for 6 platforms, creates GitHub Release
```

### Where to Find Key Things

| Need | Location |
|---|---|
| **Control plane entry point** | `cmd/orchicon/main.go` |
| **Composition root** (wires all deps) | `internal/server/server.go` |
| **Protobuf API schema** | `proto/orchicon/api/v1/` |
| **API service implementations** | `internal/{project,workflow,worker,workitem,policy,recovery,execution,auth,...}/service.go` |
| **Data-access layer** (SQL queries) | `internal/db/` |
| **Database migrations** | `db/migrations/` |
| **Declarative DB schema** (Atlas HCL) | `db/schema.hcl` |
| **Reconciler framework** | `internal/reconciler/` |
| **Task dispatch logic** | `internal/scheduler/reconciler.go` |
| **Workflow step DAG progression** | `internal/scheduler/workflow_reconciler.go` |
| **Recovery engine** | `internal/recovery/engine.go` |
| **Policy engine** (OPA/Rego) | `internal/policy/engine.go` |
| **Auth (OIDC, API keys, JWT)** | `internal/auth/` |
| **Adapter bridge** (OpenCode CLI) | `internal/opencode/` |
| **Event bus** (NATS) | `internal/eventbus/nats.go` |
| **Outbox relay** | `internal/outbox/relay.go` |
| **Telemetry setup** (OTel) | `internal/telemetry/telemetry.go` |
| **Config** (env vars) | `internal/config/config.go` |
| **Docker Compose** | `deploy/compose/docker-compose.yml` |
| **Frontend entry point** | `frontend/index.html` + `frontend/src/main.tsx` |
| **Frontend API clients** | `frontend/src/api/clients.ts` |
| **Frontend routes** | `frontend/src/routes/` |
| **Workflow canvas editor** | `frontend/src/components/workflow-editor/` |
| **Landing page** | `site/index.html` |
| **Install scripts** | `scripts/install.sh` (Linux/macOS), `scripts/install.ps1` (Windows) |
| **Dev environment controller** | `scripts/dev.sh` |
| **CI/CD workflows** | `.github/workflows/` |
| **AI agent guidelines** | `AGENTS.md` |
| **Change tracking** | `UPDATES.md` |

---

## Installation Guide

### Prerequisites

- **Go** 1.26+ (for building from source)
- **Node.js** 22+ (for frontend development)
- **Docker** + **Docker Compose** (for infrastructure services)
- **curl** + **tar** (for one-liner install)
- **buf** and **atlas** (install via `make tools`)
- **opencode** CLI (required for runtime dispatch — [install guide](https://opencode.ai))

### One-Line Install (Linux / macOS)

```bash
curl -fsSL https://orchicon.dev/install | bash
```

### One-Line Install (Windows PowerShell)

```powershell
irm https://orchicon.dev/install.ps1 | iex
```

### Install Options

| Flag | Description |
|---|---|
| `--version <tag>` | Install a specific version (e.g. `v0.2.0`). Default: latest. |
| `--install-dir <dir>` | Installation directory (default: `~/.local/bin`). |
| `--uninstall` | Remove Orchicon from the install directory. |
| `--dry-run` | Print what would happen without making changes. |
| `--clean` | Stop dev containers, remove old binary, then install latest. Preserves all data. |
| `--force-clean` / `--nuke` | Destroy Docker volumes, blob store, runtime state, then install latest. **All data lost.** |

### What Gets Installed

| Path | Contents |
|---|---|
| `<install-dir>/orchicon` | The `orchicon` binary (control plane + embedded frontend + migrations + compose) |
| `~/.local/share/orchicon/` | Runtime state, PID files, logs (`.dev/`), blob store (`data/`) |

### Build from Source

```bash
git clone https://github.com/beardedparrott/Orchicon.git
cd Orchicon
make build          # → bin/orchicon
make dev-start      # full dev environment
```

### Verify Installation

```bash
orchicon version
```

---

## User Guide

### Commands

| Command | Description |
|---|---|
| `orchicon dev start` | Start full dev stack: Docker Compose → migrations → control plane → frontend |
| `orchicon dev stop` | Stop everything (SIGTERM + Docker Compose down) |
| `orchicon dev status` | Show status of all components + endpoint health checks |
| `orchicon dev restart` | Stop then start |
| `orchicon dev logs` | Tail control-plane and frontend logs |
| `orchicon version` | Print installed version |

### Quick Start (First Run)

```bash
# 1. Install
curl -fsSL https://orchicon.dev/install | bash

# 2. Start the dev environment
orchicon dev start

# 3. Open the UI
open http://localhost:8080

# 4. Log in with built-in dev IdP
# (username/password configured in docker-compose.yml)

# 5. Create a project
# 6. Define a Worker (agent persona with system prompt, model, budget)
# 7. Create a Work Item (task) and assign it to the worker
# 8. Watch the execution in real-time
```

### Key Workflows

#### Creating and Managing Projects
1. Navigate to **Projects** → **New Project**
2. Enter a name, slug, goals (optional markdown)
3. Projects organize all workers, work items, workflows, and policies

#### Defining Workers
1. Navigate to **Workers** → **New Worker**
2. Configure: name, model reference, budget limits, permissions
3. Set the system prompt (Role, Skills, Behavior, AGENTS.md fields)
4. Publish the worker (draft → published)
5. Workers are versioned; published versions are immutable

**Canned workers** are pre-seeded in the dev tenant and available immediately:

| Worker | Purpose |
|--------|---------|
| Senior Software Engineer | Full-stack development, implements features and fixes bugs |
| PR Reviewer | Code review — finds bugs, security issues, and correctness problems |
| QA Engineer | Functional and regression testing — validates acceptance criteria |
| DevOps Engineer | Repository setup (early steps) and PR/merge after approval (late steps) |
| AI Approver | Worker-backed approval — evaluates context and outputs approve/reject |
| Principal Software Architect | Architecture design, ADR documentation, and technical strategy |

The worker identity (Role, Skills, Behavior, AGENTS.md) is included in every dispatch prompt. Workers also receive workflow-aware context: step position, iteration count, execution history, and prior issues found.

Worker output is parsed for the standard `ORCHICON WORKER SUMMARY: success|failure — <summary>` marker, which routes the workflow to the next step or triggers a loop-back.

#### Creating Work Items
1. Navigate to **Work Items** → **New Work Item**
2. Select a Project and Work Item kind (Epic → Feature → Task → Subtask)
3. Add description, acceptance criteria, and assign a worker
4. Work items form a DAG with dependency edges (cycle detection enforced)

#### Building Workflows (Visual Editor)
1. Navigate to **Workflows** → **New Workflow**
2. Open the React Flow canvas editor
3. Drag steps from the palette: Task, Decision, Approval, Parallel, Loop Decision, Work Item, Project, Policy
4. Connect steps with edges (directed acyclic graph with loop-back and success edge support)
5. Configure each step's properties in the Properties Panel
6. Save draft, then publish when ready
7. Start a workflow run and watch step-by-step progression

#### Approval Gates
The **Approval** step kind blocks a workflow at a human (or AI) review gate. It handles loop-back natively — no separate loop_decision node needed.

**Human approval (default):**
1. The step waits in `approval_pending` status until a human reviews it
2. Navigate to **Approvals** in the sidebar or open the workflow run view
3. Review the upstream context (worker summary, touched files, acceptance criteria)
4. Click **Approve** or **Reject** with an optional reason
5. On rejection, the workflow loops back to the loop_branch step (if configured)
6. On approval, the workflow proceeds to the next downstream step

**Worker-backed approval (AI Approver):**
1. In the step's Properties Panel, set **Reviewer** to **Worker**
2. Select an approver worker (e.g. AI Approver — an opinionated worker that outputs approve/reject)
3. The step dispatches the approver worker like a task step
4. The worker's `ORCHICON WORKER SUMMARY` output determines the decision:
   - `success` → approved, workflow proceeds
   - `failure` → rejected, workflow loops back (if loop_branch configured)
5. The decision is visible in the Approvals list alongside human reviews

**Loop-back configuration:**
- **Loop branch**: Set by connecting the loop outlet (right, rose handle) to a topologically-prior step
- **Max rejections**: How many times the workflow can loop back before failing the run
- The step N of M context in the worker prompt tells each worker their position in the DAG

**Execution history:**
Workers now receive full execution context including:
- Their position (step N of M) via topological sort
- Which iteration of a loop-back they are on
- The execution history timeline of all prior steps and their results
- Previous issues found by reviewers

#### Viewing Execution Results
1. Navigate to **Executions** for the full list
2. Click an execution to see: streaming output, conversation, cost, duration
3. Follow-up chat available for continued interaction
4. Filter, search, sort, and bulk-delete executions

#### Recovery from Failures
1. When an execution fails, recovery can be triggered
2. Default recovery: capture → summarize → preserve → review → plan → resume
3. L1 → L2 → L3 escalation with bounded auto-relax
4. View recovery timeline in the **Recovery** section

#### Policy Management
1. Navigate to **Policies** → **New Policy**
2. Write Rego rules for: admission, dispatch, budget, approval, recovery, completion
3. Narrowest-scope-first evaluation; default is allow
4. Full Rego traces available for `ExplainDecision`

#### Telemetry & Cost
1. Navigate to **Telemetry** for: traces, metrics, logs dashboard
2. Embedded SigNoz UI available at `/signoz`
3. Cost Explorer: per-provider/model spend with drill-down (Project → Task → Execution → Model)
4. **By Workflow** tab: cost broken down per workflow run with per-step detail
5. Credits tab showing tenant-level usage

#### Settings
1. Navigate to **Settings** (replaces the former Preferences page)
2. **Appearance**: light/dark mode toggle with 20 theme variants (10 light + 10 dark)
3. **Defaults → Default models**:
   - **Default worker model**: fallback when a worker version has no `model_ref` set. If both are empty, dispatch fails (no hardcoded fallback).
   - **Default Ask Orchicon model**: placeholder for the forthcoming Ask Orchicon assistant.
4. **Defaults → Recovery stall parameters**: per-execution stall thresholds stored in the DB and read at dispatch time. Each field has an env-var override (`ORCHICON_STALL_*`) for dev debugging.

### Authentication

- **Local dev**: Built-in OIDC provider (HS256) — no external IdP needed
- **Production**: Real OIDC issuer with authorization-code flow
- **API keys**: SHA-256 hashed, least-privilege scopes for headless/CI clients
- **Frontend**: Access tokens in memory; refresh tokens in HttpOnly cookies

---

## Development Guide

### Local Development Loop

The fastest local development cycle:

```bash
scripts/install-local.sh          # builds frontend + Go binary
orchicon stop && orchicon start   # restart with new binary
```

The binary embeds everything via `go:embed` — no separate build steps needed.

### Manual Development Setup

```bash
# Terminal 1: Infrastructure
make up                           # Start Postgres, NATS, ClickHouse, SigNoz, OTel

# Terminal 2: Migrations + Control Plane
make migrate                      # Apply database migrations
make run                          # Run control plane on :8080

# Terminal 3: Frontend (optional, for hot-reload)
make fe-install && make fe-dev    # Vite dev server on :5173
```

### Makefile Targets

| Target | Description |
|---|---|
| **Tooling** | |
| `tools` | Install `buf` and `atlas` CLI tools |
| **Codegen** | |
| `gen` | Generate Go + TypeScript from Protobuf (`buf generate`) |
| `lint` | Lint Protobuf schema (`buf lint`) |
| `proto` | Lint + generate combined |
| **Go Control Plane** | |
| `build` | Build binary to `bin/` |
| `run` | Run from source |
| `test` | Run Go tests |
| `vet` | Run `go vet` |
| `tidy` | Run `go mod tidy` |
| **Database** | |
| `migrate` | Apply pending Atlas migrations |
| `migrate-diff` | Generate new migration from `db/schema.hcl` |
| `migrate-hash` | Recompute Atlas migration directory hash |
| `rls-check` | Verify every `tenant_id` table has RLS policy |
| **Docker Compose** | |
| `up` | Start dev stack (PG, NATS, ClickHouse, OTel, SigNoz) |
| `down` | Stop dev stack |
| `logs` | Tail dev-stack logs |
| `ps` | Show dev-stack status |
| `nuke` | Stop + delete all volumes |
| **Frontend** | |
| `fe-install` | Install frontend dependencies |
| `fe-dev` | Start Vite dev server |
| `fe-build` | Build for production |
| `fe-lint` | Lint frontend |
| **Dev Control** | |
| `dev-start` | Start full dev environment (via `scripts/dev.sh`) |
| `dev-stop` | Stop full dev environment |
| `dev-status` | Show status of all components |
| `dev-restart` | Restart full dev environment |
| `dev-logs` | Tail control-plane + frontend logs |
| **CI** | |
| `ci` | Full CI gate: lint → gen → vet → test → rls-check |

### Code Generation

Protobuf schema (`proto/`) is the single source of truth:

```bash
make gen    # buf generate → api/gen/go + frontend/src/api/gen
```

This generates Go handlers and TypeScript Connect-ES clients. Generated code is committed to the repo.

### Database Migrations

Migrations are managed by Atlas (declarative):

```bash
make migrate        # Apply pending migrations
make migrate-diff   # Generate new migration from schema changes
make migrate-hash   # Recompute migration directory hash
```

Key rules:
- Migrations are forward-only (no down migrations)
- Every `ALTER TABLE ADD COLUMN` must use `IF NOT EXISTS`
- Every `REFERENCES` column must carry `ON DELETE SET NULL` or `ON DELETE CASCADE`
- Every `tenant_id` column must have an RLS policy

### Testing

```bash
# Go tests
make test

# Full CI gate
make ci

# RLS policy check (must pass before merge)
make rls-check
```

### Verification Checklist

Before marking any change complete:

1. **`make ci` passes** — buf lint, codegen, go vet/test, RLS gate
2. **Dev stack starts healthy** — `make up` then `make ps` shows all 6 containers healthy (postgres, nats, clickhouse, signoz-schema-migrator, otel-collector, signoz)
3. **Migrations apply cleanly** — `make migrate` + `make rls-check`
4. **Control plane boots** — `make build && make run`, then `curl http://localhost:8080/healthz` returns `{"status":"ok"}` in <2s
5. **Frontend renders** — `curl http://localhost:5173/` returns HTTP 200

### Key Architecture Invariants

1. No business logic in the frontend — the UI reflects server state
2. No hand-written API URLs — use the generated Connect-ES client
3. No mutations outside the transactional outbox pattern
4. No raw SQL outside the data-access layer (`internal/db/`)
5. Every `tenant_id` table must have an RLS policy
6. Adapters never touch Postgres or NATS directly — gRPC stream only
7. No automatic model failover — the human defines the exact model
8. Recovery is opt-out, not opt-in
9. Migrations are forward-only

### Styling & Conventions

- **Go**: Standard library style, `internal/` packages, pgx parameterized queries
- **TypeScript**: TanStack Query for server state, Zustand for UI state, shadcn/ui components
- **CSS**: Tailwind utility classes with 15 CSS-variable themes (10 light + 10 dark)
- **Protobuf**: `proto/orchicon/api/v1/` for public API, `proto/orchicon/adapter/v1/` for runtime contract

---

## Deployment

### Cloudflare Pages (Landing Page)

The static landing page at `orchicon.dev` is deployed via Cloudflare Pages:

1. Push to `main` triggers auto-deploy
2. `scripts/build-site.sh` copies `scripts/install.sh` → `site/install` and `scripts/install.ps1` → `site/install.ps1`
3. Cloudflare Pages builds with: `bash scripts/build-site.sh` from repo root, output dir = `site/`

**Build settings** (configured in Cloudflare Dashboard):
- Build command: `bash scripts/build-site.sh`
- Build output directory: `site`
- Root directory: (blank = repo root)

See [`CLOUDFLARE_SETUP.md`](./CLOUDFLARE_SETUP.md) for the one-time setup guide.

### GitHub Releases (Binary Distribution)

1. Tag push matching `v*.*.*` triggers `release.yml`
2. Builds binaries for 6 platforms: linux/amd64, linux/arm64, darwin/amd64, darwin/arm64, windows/amd64, windows/arm64
3. Creates a GitHub Release with platform-specific archives
4. The install scripts download from the latest release

### Release Workflow

1. Create a PR with the `release` label
2. Merge to `main` — `auto-release.yml` bumps the version tag
3. `release.yml` builds and publishes binaries

---

## Environment Variables Reference

| Variable | Default | Purpose |
|---|---|---|
| `ORCHICON_HTTP_ADDR` | `:8080` | HTTP listen address (frontend + API) |
| `ORCHICON_GRPC_ADDR` | `:9090` | gRPC listen address |
| `ORCHICON_POSTGRES_DSN` | `postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable` | PostgreSQL connection string |
| `ORCHICON_NATS_URL` | `nats://localhost:4222` | NATS server URL |
| `ORCHICON_OTEL_ENDPOINT` | `localhost:4317` | OTel collector gRPC endpoint |
| `ORCHICON_SIGNOZ_URL` | `http://localhost:3301` | SigNoz query-service URL |
| `ORCHICON_CLICKHOUSE_DSN` | `http://signoz:signoz@localhost:8123` | ClickHouse HTTP DSN |
| `ORCHICON_MODE` | `local` | Operating mode: `local` or `production` |
| `ORCHICON_BLOB_STORE` | `local` | Blob store backend: `local` or `s3` |
| `ORCHICON_OIDC_ISSUER` | `local` | OIDC issuer URL (or `local` for dev IdP) |
| `ORCHICON_OIDC_CLIENT_ID` | (none) | OIDC client ID |
| `ORCHICON_OIDC_CLIENT_SECRET` | (none) | OIDC client secret |
| `ORCHICON_SIGNING_KEY` | (auto-generated) | JWT signing key (required in production) |
| `ORCHICON_SIMULATE_ADAPTER` | `false` | Enable adapter simulation mode (no-op dispatch) |
| `ORCHICON_STALL_NO_PROGRESS_WINDOW` | `300s` | Time without step_finish/token progress before stall (overrides DB setting) |
| `ORCHICON_STALL_NO_FILE_DIFF_WINDOW` | `15m` | Time without file modifications before stall (overrides DB setting) |
| `ORCHICON_STALL_REPETITION_COUNT` | `5` | Repeated tool calls before stall within window (overrides DB setting) |
| `ORCHICON_STALL_REPETITION_WINDOW` | `300s` | Window for repetition count detection (overrides DB setting) |
| `SIGNOZ_IDENTN_IMPERSONATION_ENABLED` | `true` | SigNoz impersonation mode (disable for enterprise) |
| `SIGNOZ_USER_ROOT_ENABLED` | `true` | SigNoz root user mode |

---

## Troubleshooting

### Dev Stack Won't Start

| Symptom | Likely Cause | Fix |
|---|---|---|
| Containers stay `unhealthy` | Missing Docker volume or config | Run `make nuke && make up` to reset volumes |
| ZooKeeper in `make ps` | Outdated ClickHouse config | Should use embedded Keeper — check `clickhouse-cluster.xml` has `<keeper_server>`, not `<zookeeper>` |
| Control plane won't connect to DB | Postgres not healthy yet | Wait for `make ps` to show all healthy; check `ORCHICON_POSTGRES_DSN` |
| NATS healthcheck fails | Missing `-m 8222` flag | NATS HTTP monitor serves healthz on 8222 — ensure compose file has it |

### Control Plane Issues

| Symptom | Likely Cause | Fix |
|---|---|---|
| Boot takes >2s | OTel blocking dial | Should use `grpc.NewClient` (non-blocking) — check `internal/telemetry/telemetry.go` |
| `"/healthz"` returns non-200 | Missing handler | Check `internal/api/api.go` mounts healthz before Connect handlers |
| Migrations fail | Non-idempotent migration | Every `ADD COLUMN` must use `IF NOT EXISTS` |
| RLS check fails | New table lacks RLS | Add `CREATE POLICY tenant_isolation` to the migration |

### Frontend Issues

| Symptom | Likely Cause | Fix |
|---|---|---|
| Blank page / no API responses | Vite proxy not configured | `vite.config.ts` must proxy `/orchicon.api.v1*` to `:8080` |
| SigNoz iframe blank | Reverse proxy gzip issue | SigNoz proxy must decompress gzip before HTML rewrite (see `internal/api/signoz_proxy.go`) |
| React Error #310 (hook mismatch) | Conditional hook call | Ensure hooks are called unconditionally — check for early returns before hooks |
| Workflow canvas not loading | Missing `reactflow/dist/style.css` | Import React Flow CSS in the route component |

### Runtime Issues

| Symptom | Likely Cause | Fix |
|---|---|---|
| Worker execution never starts | `opencode` not in PATH | Install opencode CLI: `curl -fsSL https://opencode.ai/install | bash` |
| System prompt not sent to worker | Wrong env var | Must use `OPENCODE_CONFIG_CONTENT` with custom agent, not `OPENCODE_SYSTEM_PROMPT` |
| Loop decision stuck | Superseded step run conflict | Workflow reconciler must skip `SupersededBy != ""` runs |
| Stale decisions leaking across runs | Previous `_decision` file | Clear `.orchicon/<run_id>/` files between steps |

---

## Contributing

### Branch Workflow

1. Never commit to `main` — the pre-commit hook enforces this
2. Create a branch: `feat/`, `fix/`, `chore/`, `refactor/`, `docs/`, or `test/` prefix
3. Before starting work: `git tag -a v0.1.<next> -m "v0.1.<next>"` then bump version
4. Commit early and often with clear present-tense messages
5. Before PR: update `README.md` (Last Release Changes) and `UPDATES.md`
6. Ask for approval before creating a PR
7. PRs must carry the `release` label for auto-release

### Local Pre-commit Hook

```bash
#!/bin/sh
# .git/hooks/pre-commit
branch="$(git symbolic-ref --short HEAD)"
if [ "$branch" = "main" ] || [ "$branch" = "master" ]; then
  echo "ERROR: Direct commits to $branch are blocked!"
  exit 1
fi
```

### Code Standards

- No business logic in frontend — UI reflects server state
- No hand-written API URLs — use generated Connect-ES clients
- No mutations outside the transactional outbox
- No raw SQL outside `internal/db/`
- Parameterized queries only (pgx `$1`, `$2`, ...)
- Validate input at the API boundary — trim, bound-check, regex-validate
- Every `tenant_id` table needs RLS
- No secrets in code, commits, or logs

---

## License

Copyright © 2026 beardedparrott. All rights reserved.

This software is provided free of charge for personal and non-commercial use. You may use, copy, and modify it for your own non-commercial purposes. Redistribution, sublicensing, or integration into commercial products that generate revenue requires explicit written permission from the owner. See the [LICENSE](./LICENSE) file for the full terms.
