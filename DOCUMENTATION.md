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

The frontend is a TypeScript/React SPA with a visual React Flow workflow editor, real-time execution streaming, and an embedded Grafana telemetry dashboard (Tempo + Loki + VictoriaMetrics). The entire stack — PostgreSQL, NATS JetStream, OpenTelemetry, Grafana — runs in a single container (`orchicon container` PID-1 supervisor), managed by `scripts/container.sh` or launched directly from the GHCR image `ghcr.io/beardedparrott/orchicon`.

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
| Observability | Grafana (Tempo + Loki + VictoriaMetrics) | Traces, metrics, logs dashboard |
| Trace Backend | Grafana Tempo 2.7 | Local-disk trace storage (OTLP ingest) |
| Log Backend | Grafana Loki 3.4 | Log aggregation (OTLP ingest, filesystem storage) |
| Metric Backend | VictoriaMetrics | PromQL-compatible metrics (remote-write ingest) |
| OTel Collector | otel-contrib 0.119 | Pipeline fan-out: traces → Tempo, logs → Loki, metrics → VM |
| Object Storage | Local filesystem or S3 | Blob store abstraction |
| Policy Engine | OPA v1 (Rego) | Governance policy evaluation |
| Runtime Adapter | OpenCode CLI | Default AI agent runtime (pluggable via gRPC) |
| Deployment | Single container (`deploy/container/`) | `orchicon container` PID-1 supervisor runs the whole stack in one image (GHCR `ghcr.io/beardedparrott/orchicon`); `scripts/container.sh` manages dev (`orchicon-cnt-dev`, :8080/:3002) and prod (`orchicon-cnt-prod`, :8091/:3003) instances |

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
        Tempo[(Tempo<br/>traces)]
        Loki[(Loki<br/>logs)]
        VM[(VictoriaMetrics<br/>metrics)]
    end

    subgraph "Observability Stack"
        OTel[OTel Collector]
        Grafana[Grafana UI]
    end

    subgraph "Runtime"
        RuntimeDaemon[orchicon runtime-daemon<br/>host · owns Docker socket]
        RuntimeContainer[orchicon-runtime-&lt;runID&gt;<br/>per active workflow run]
        OpenCode[OpenCode CLI<br/>inside runtime container]
        FutureRuntime[Future Runtimes<br/>gRPC Sidecar]
    end

    subgraph "Frontend (Browser)"
        SPA[React SPA :5173<br/>Vite / TanStack Router]
        ReactFlow[React Flow<br/>Workflow Editor]
        GrafanaIFrame[Embedded Grafana]
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
    AdapterBridge --> RuntimeDaemon
    RuntimeDaemon --> RuntimeContainer
    RuntimeContainer --> OpenCode
    AdapterBridge -.-> FutureRuntime
    AIGateway --> OpenCode
    Connect --> Policy
    Connect --> RecoveryEngine
    Telemetry --> OTel
    OTel --> Tempo
    OTel --> Loki
    OTel --> VM
    Grafana --> Tempo
    Grafana --> Loki
    Grafana --> VM
    Grafana -.-> GrafanaIFrame
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
    A->>RT: Dispatch into workflow runtime container (orchicon runtime-daemon)
    RT->>RT: orchicon runtime-supervisor runs opencode CLI
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

3. **Single Binary** — The Go binary embeds the single-container runtime configs (`deploy/container/configs/`), SQL migrations, and the built frontend SPA via `go:embed`. No external dependencies at runtime beyond Docker. The **single container** (`orchicon container` runs the whole stack as PID-1 — §Single-Container Deployment) is the only full-stack deployment; the same binary also runs headless via `orchicon serve`.

4. **Non-blocking OTel** — The OpenTelemetry pipeline uses `grpc.NewClient` (non-blocking dial), so the control plane boots in <2 seconds even when the OTel collector is not yet healthy.

5. **Connect-ES** — Single Protobuf schema generates both Go server and TypeScript client code. Supports unary RPC, server-streaming, and client-streaming over the same interface.

6. **RLS-backed Tenant Isolation** — Every tenant-scoped table has a PostgreSQL Row-Level Security policy as a backstop. The data-access layer also injects `app.tenant_id` via session variables.

7. **Adapter Bridge Pattern** — Runtimes are pluggable gRPC sidecars. The built-in adapter drives the OpenCode CLI — locally as a subprocess (headless `orchicon serve`) or, for workflow-run executions, inside the per-workflow runtime container via `orchicon runtime-daemon` — parsing its JSON telemetry output. Future runtimes implement the `orchicon.adapter.v1` gRPC contract.

8. **Worker Sandboxing (layered defense)** — Every worker execution is contained by three layers, all applied to **every** worker automatically and enforced even under `--auto`:
    - **opencode permission deny rules** (`permissionRules()` in `internal/opencode/config.go`) injected via `OPENCODE_CONFIG_CONTENT`. `external_directory` is `deny` (any tool touching a path outside the project's `--dir` is blocked), and an extensive `bash` deny list blocks `rm`/`sudo`/`dd`/`mkfs*`/`fdisk`/`parted`/`shred`/`wipefs`/LVM tools, root-wide `chmod -R`/`chown -R`, `/dev/sd*` redirection, shell-construct smuggling variants (`(rm -rf /) &`, `{ rm -rf /; }`, chained `;`/`&&`/`&`/`|`), and download-and-execute. No catch-all `*` allow rule is emitted.
    - **OS-level execution guard** (`internal/guard/guard.go`) — shims dangerous binaries (`rm`, `sudo`, `dd`, `mkfs*`, `fdisk`, `parted`, `shred`, `wipefs`, LVM, `chmod`, `chown`, `mv`, `cp`, `ln`) ahead of the worker's PATH. Any process the worker spawns — including a python TUI, `os.system`, or `subprocess.run` issuing `rm -rf /` — resolves the command through the shim and is refused when it targets `/`, `~`, `$HOME`, `/home`, or any path outside the project directory. This closes the subprocess hole that opencode's rules cannot see (a destructive command issued inside a python TUI only ever looks like `python tui.py` to opencode). This is defense-in-depth, not a container: a worker that resolves the real binary by absolute path or writes its own tool still escapes. The containment layer for those cases is the workflow runtime container (§Workflow Runtime Containers) — every execution runs inside a short-lived, root-free container, so even a fully compromised worker cannot touch the host.
    - **Worker prompt context** — every canned worker's AGENTS.md carries a "Safety rules" block (see `internal/db/seed_workers.go`) forbidding destructive commands, destructive "security testing", and scope creep. Review/QA workers additionally run the **safety lint** — Semgrep (a cross-platform Python CLI, works on Linux/macOS/Windows) with Orchicon's destructive-command ruleset — by running `semgrep scan --config .orchicon/semgrep_orchicon.yml --error .` from the project root. The ruleset and a `.semgrepignore` are written into every project by the control plane (`internal/opencode/lint.go`).

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
├── assets.go                    # go:embed: container configs, migrations, frontend
├── buf.gen.yaml                 # Buf codegen config (Go + TypeScript)
├── buf.yaml                     # Buf lint config
├── CLOUDFLARE_SETUP.md          # Cloudflare Pages one-time setup guide
├── DOCUMENTATION.md             # ← This file: comprehensive docs
├── LICENSE                      # Custom license (non-commercial)
├── Makefile                     # All targets: build, test, gen, container-*, ci
├── opencode.jsonc               # Opencode tool configuration
├── README.md                    # Project introduction & quick start
├── UPDATES.md                   # Per-PR change tracking
├── wrangler.toml                # Cloudflare Pages project config
│
├── cmd/
│   └── orchicon/                # Go binary entry point
│       ├── main.go              # Subcommand dispatch (serve, container, runtime-*, db, version, etc.)
│       ├── container.go         # `orchicon container` PID-1 supervisor
│       ├── serve.go             # `orchicon serve` headless control plane
│       ├── serve_state.go       # Detached serve state (PID file, logs)
│       ├── runtime.go           # `runtime-daemon` / `runtime-supervisor` / `runtime-client`
│       ├── procattr_unix.go     # Unix process attributes for background fork
│       └── procattr_windows.go  # Windows process attributes
│
├── deploy/
│   ├── container/               # Single-container image (Dockerfile + embedded configs)
│   └── runtime/                 # Workflow runtime container image (toolchain base + :gui variant)
│       ├── Dockerfile           #   base image (baked toolchain, no-root model)
│       └── Dockerfile.gui       #   :gui variant (headless GUI libs) + custom-image template
│
├── internal/
│   ├── adapter/                 # RuntimeAdapterService (list adapters, capabilities)
│   ├── aigateway/               # AI Gateway: model/MCP discovery, usage recording
│   ├── api/                     # Connect handler mounting, Grafana reverse proxy
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
│   ├── guard/                   # OS-level execution guard shim (leaf package)
│   ├── runtime/                 # Workflow runtime containers: daemon client, in-container agent, lifecycle, image build
│   ├── runtimeimage/            # RuntimeImageService: image spec CRUD + build orchestration
│   ├── outbox/                  # Outbox event types + background relay
│   ├── policy/                  # OPA/Rego policy engine + PolicyService
│   ├── project/                 # ProjectService + validation
│   ├── rbac/                    # RBAC Connect interceptor
│   ├── reconciler/              # Reconciler framework (work queue, leader election)
│   ├── recovery/                # Recovery engine + RecoveryService
│   ├── scheduler/               # TaskReconciler, WorkflowReconciler, ScheduledRunReconciler
│   ├── server/                  # Composition root (wires all dependencies)
│   ├── telemetry/               # OTel setup, Grafana-stack query client, telemetry service
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
│   └── container/
│       ├── Dockerfile                    # Single-container image (PID-1 supervisor entrypoint)
│       ├── .dockerignore                 # Build-context excludes
│       └── configs/                      # Embedded runtime configs (@DATA_DIR@ placeholders):
│           ├── tempo.yaml                #   Tempo (OTLP ingest on 14317/14318)
│           ├── loki.yaml                 #   Loki (gRPC on 9096)
│           ├── otel-collector.yaml       #   Collector fan-out to localhost backends
│           ├── grafana.ini               #   Grafana (sub-path + anonymous)
│           └── grafana-provisioning/     #   Grafana datasources + Orchicon dashboard
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
│           ├── runtime-images{,_.new,_.$id}.tsx
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
│   ├── install.ps1              # Windows PowerShell installer (provisions WSL2, runs the stack inside it)
│   ├── install-local.sh         # Build & install from local source to ~/.local/bin
│   ├── container.sh             # Dev/prod single-container instances (build/up/down/status/logs)
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
| **Runtime image service** (build specs) | `internal/runtimeimage/` |
| **Runtime containers** (daemon/client/agent) | `internal/runtime/` |
| **Event bus** (NATS) | `internal/eventbus/nats.go` |
| **Outbox relay** | `internal/outbox/relay.go` |
| **Telemetry setup** (OTel) | `internal/telemetry/telemetry.go` |
| **Config** (env vars) | `internal/config/config.go` |
| **Single container** (PID-1 supervisor) | `cmd/orchicon/container.go` + `deploy/container/` |
| **Frontend entry point** | `frontend/index.html` + `frontend/src/main.tsx` |
| **Frontend API clients** | `frontend/src/api/clients.ts` |
| **Frontend routes** | `frontend/src/routes/` |
| **Workflow canvas editor** | `frontend/src/components/workflow-editor/` |
| **Landing page** | `site/index.html` |
| **Install scripts** | `scripts/install.sh` (Linux/macOS), `scripts/install.ps1` (Windows → provisions WSL2, installs the Linux binary inside the distro) |
| **Container instance controller** | `scripts/container.sh` |
| **CI/CD workflows** | `.github/workflows/` |
| **AI agent guidelines** | `AGENTS.md` |
| **Change tracking** | `UPDATES.md` |

---

## Installation Guide

### Prerequisites

- **Go** 1.26+ (for building from source)
- **Node.js** 22+ (for frontend development)
- **Docker** (for the single-container deployment; the headless `orchicon serve` binary needs no external services)
- **curl** + **tar** (for one-liner install)
- **buf** and **atlas** (install via `make tools`)
- **opencode** CLI (required for runtime dispatch — [install guide](https://opencode.ai))

> **Orchicon never ships runtime adapter CLIs in its images.** opencode (and, in the future, Claude Code / Codex) is installed by the operator **on the host** and bind-mounted into the containers at runtime — the images contain no adapter binary. This keeps the product redistributable regardless of an adapter's license (Claude Code's terms, for example, prohibit bundling it with a product). The installer verifies opencode is present on the host and fails with a clear message if it is not; the adapter resolves it from `PATH` or `~/.opencode/bin`.

### One-Line Install (Linux / macOS)

```bash
curl -fsSL https://orchicon.dev/install | bash
```

The installer downloads the binary, then runs `orchicon install` to set up **everything**: pull the published images (`ghcr.io/beardedparrott/orchicon` + `orchicon-runtime`), start the host-side runtime daemon, launch the single-container instance, and print how to connect / start / stop. Pass `--no-setup` to install only the binary (headless / CI).

### One-Line Install (Windows PowerShell — via WSL2)

```powershell
irm https://orchicon.dev/install.ps1 | iex
```

Orchicon's runtime layer (the runtime daemon, its unix socket, and the container mounts) is **POSIX-only** — there is no native Windows port. On Windows the whole stack therefore runs inside **WSL2**, and the installer orchestrates it:

1. **WSL2 provisioning** — the script detects WSL and a Linux distro, ensures WSL2 is the default version, and prints exact next steps if WSL or a distro is missing (Windows 10 21H2+ / Windows 11: run `wsl --install` in an admin shell and reboot).
2. **Docker check inside WSL** — confirms `docker version` works inside the distro (Docker Desktop with WSL2 integration, or Docker Engine installed in the distro) and prints setup steps if not.
3. **Linux binary** — it downloads the **Linux** release asset (`orchicon_<ver>_linux_<arch>.tar.gz`, arch mapped from the Windows processor) and installs it inside the distro (default `~/.local/bin`); it never downloads the Windows binary.
4. **One-command setup** — it runs `orchicon install` inside WSL: pull the published images (`ghcr.io/beardedparrott/orchicon` + `orchicon-runtime`), start the runtime daemon, launch the single-container instance, wait for health.
5. **Connection info** — WSL2 forwards `localhost`, so it prints the Windows-visible URLs: `http://localhost:8080` (control plane) and `http://localhost:3002` (Grafana). If the URLs do not answer, check Windows Defender Firewall or add a `netsh interface portproxy` port forward.

**Prerequisites:**

- **Windows 10 21H2+ / Windows 11**. First-time WSL users run `wsl --install` from an elevated PowerShell and reboot; the installer guides through this if it is not yet set up.
- **Docker Desktop** with the **WSL integration** enabled for your distro (Settings → Resources → WSL Integration), or Docker Engine running inside the distro.

**Project directories are WSL paths.** The UI's `project_dir` is a host path. Inside WSL2 a Windows project `C:\Users\you\projects\Foo` is mounted by Docker Desktop at `/mnt/c/Users/you/projects/Foo` — enter that **WSL path** in the project form. There is no Windows↔WSL path translation layer; the UI accepts whatever path the plane's filesystem (WSL) can resolve.

**Options** (PowerShell form of the Linux flags): `-Version`, `-InstallDir` (a **WSL** path, default `~/.local/bin`), `-NoSetup`, `-Uninstall` (stops the container instances and removes the binary; the WSL distro is left intact), `-Clean`, `-ForceClean`, `-DryRun`.

> **Not verified on real Windows** — the WSL2 installer ships as-is for testing. The underlying flow (`orchicon install`) is the identical Linux code path exercised on Linux hosts; see the PR description for what a Windows tester should run.

### Install Options

| Flag | Description |
|---|---|
| `--version <tag>` | Install a specific version (e.g. `v0.2.0`). Default: latest. |
| `--install-dir <dir>` | Installation directory (default: `~/.local/bin`). |
| `--uninstall` | Remove Orchicon from the install directory. |
| `--dry-run` | Print what would happen without making changes. |
| `--clean` | Stop any running instance, remove old binary, then install latest. Preserves all data. |
| `--force-clean` / `--nuke` | Remove container instances + data volumes, blob store, runtime state, then install latest. **All data lost.** |

### What Gets Installed

| Path | Contents |
|---|---|
| `<install-dir>/orchicon` | The `orchicon` binary (control plane + embedded frontend + migrations + container configs). On Windows this lives **inside the WSL2 distro** (`~/.local/bin/orchicon`). |
| `~/.local/share/orchicon/` | Runtime state, PID files, logs (`.dev/`), blob store (`data/`). On Windows, under the WSL distro's home. |

### Build from Source

```bash
git clone https://github.com/beardedparrott/Orchicon.git
cd Orchicon
make build                  # → bin/orchicon (headless control plane + PID-1 supervisor)
make container-build        # build the single-container image
scripts/container.sh up dev # start the dev instance (http://localhost:8080)
```

Or run the image directly without building from source:

```bash
docker run -p 8080:8080 -p 3002:3000 ghcr.io/beardedparrott/orchicon
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
| `orchicon container` | Run the whole stack as PID-1 (single-container image) |
| `orchicon install` | One-command setup: pull images, start the runtime daemon, launch the container, print connection info |
| `orchicon runtime-daemon` | Host process owning the Docker socket; spawns per-workflow runtime containers |
| `orchicon runtime-supervisor` | Runtime container PID 1 (streams `opencode run`) |
| `orchicon runtime-client` | Forwards dispatches into the runtime container |
| `orchicon mcp` | Start the MCP stdio server (exposes the Ask Orchicon tool registry; registered in opencode runs by default) |
| `scripts/container.sh` | Build / up / down / status / logs / ps / runtime-daemon / runtime-stop for dev + prod container instances |
| `orchicon serve` | Run the control plane with embedded frontend (headless, migrations on boot) |
| `orchicon serve --detach` / `--stop` | Manage a background `serve` instance (PID file; logs in `.dev/logs/`) |
| `orchicon db` | Database maintenance: `backup`, `restore`, `list`, `prune` |
| `orchicon version` | Print installed version |

### Quick Start (First Run)

```bash
# 1. Install
curl -fsSL https://orchicon.dev/install | bash

# 2. Start the full stack in a single container
scripts/container.sh up dev

# 3. Open the UI
open http://localhost:8080

# 4. Log in with the built-in dev IdP
# (or run the GHCR image directly: docker run -p 8080:8080 -p 3002:3000 ghcr.io/beardedparrott/orchicon)

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

An execution is only reported `succeeded` when the run completes with the final model step fully delivered (`step_finish` received). opencode's `--format json` emits the entire model response as ONE stdout line, and a scanner cap smaller than the response used to drop that line **and** every event after it (a `bufio.Scanner` is permanently broken after `ErrTooLong`) — so a large final answer made an otherwise-successful execution come back `succeeded` with **empty output**, and the loop_decision step saw no `_decision` signal and re-asked until it failed. Two fixes close this: the runtime path now uses the same 1MB line cap as the local path (was 64KB), and the adapter tracks `step_start` vs `step_finish` counts so a clean exit with an unpaired `step_start` is downgraded to a failure (`execution ended before the final model step completed (model response stream truncated or event dropped)`) instead of a silent success.

#### Creating Work Items
1. Navigate to **Work Items** → **New Work Item**
2. Select a Project and Work Item kind (Epic → Feature → Task → Subtask)
3. Add description, acceptance criteria, and assign a worker
4. Work items form a DAG with dependency edges (cycle detection enforced)

**Status while bound to a workflow run:** a work item that kicks off (or is bound to) a workflow run is a **shared input reference** — every step reads the same ticket (title, description, acceptance criteria, upstream context) and produces its own execution and output. `StartWorkflow` moves it to `running`, and it stays `running` for the whole run; it is never mutated per-step (no `assigned_worker_ref`, `workflow_step_id`, or prompt writes, no `ready`/`assigned`/`recovering` flips). The item reaches `succeeded`/`failed` only when the whole run completes/fails. Because the ticket is never written per-step, **two steps bound to the same ticket can run in parallel** — each step run owns its own execution (`worker_execution_id`) and its own results. When the run ends, the ticket's `results` carry a run-level narrative (`_run_narrative`) aggregating each step's summary/decision/issues plus every recovery episode.

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
1. When a step's execution fails, recovery is triggered automatically (opt-out, not opt-in)
2. **Recovery is scoped per failing step run** — the work item is a shared input reference and is never flipped to `recovering`. Each failing step run goes `recovering`, gets its own recovery cycle (capture → summarize → preserve → review → plan → resume), and re-dispatches with a fresh execution once recovery completes. Two steps failing on the same ticket each get their own recovery.
3. The recovery summary is written to the step run (`_recovery_summary`) so the replacement execution's prompt includes the failure context (prevents repeating the same failure)
4. L1 → L2 → L3 escalation with bounded auto-relax
5. View recovery timeline in the **Recovery** section; the ticket's run-level narrative (`_run_narrative.recoveries`) lists every episode

#### Policy Management
1. Navigate to **Policies** → **New Policy**
2. Write Rego rules for: admission, dispatch, budget, approval, recovery, completion
3. Narrowest-scope-first evaluation; default is allow
4. Full Rego traces available for `ExplainDecision`

#### Telemetry & Cost
1. Navigate to **Telemetry** for: traces, metrics, logs dashboard
2. Embedded Grafana UI available at `/grafana` (Tempo / Loki / VictoriaMetrics)
3. Cost Explorer: per-provider/model spend with drill-down (Project → Task → Execution → Model)
4. **By Workflow** tab: cost broken down per workflow run with per-step detail
5. Credits tab showing tenant-level usage

#### Settings
1. Navigate to **Settings** (replaces the former Preferences page)
2. **Appearance**: light/dark mode toggle with 20 theme variants (10 light + 10 dark)
3. **Defaults → Default models**:
   - **Default worker model**: fallback when a worker version has no `model_ref` set. If both are empty, dispatch fails (no hardcoded fallback).
   - **Default Ask Orchicon model**: model used by the Ask Orchicon conversational agent. If empty, dispatch will fail.
 4. **Defaults → Recovery stall parameters**: per-execution stall thresholds stored in the DB and read at dispatch time. Each field has an env-var override (`ORCHICON_STALL_*`) for dev debugging.
 5. **Defaults → Execution liveness reaper**: tuning for the execution-liveness reaper (the sweep that fails executions whose runtime process is gone). The liveness probe can false-negative on a transient docker/socket hiccup, so an execution is only reaped once it is **older than the grace window** (default 60s) **and** has been reported not-alive for **consecutive-failures** checks in a row (default 3). Env overrides: `ORCHICON_REAP_GRACE_SECONDS`, `ORCHICON_REAP_CONSECUTIVE_FAILURES`.
 6. **Defaults → Execution transport resilience**: the exec stream between the control plane and the runtime supervisor can break on a transient socket/docker hiccup. The execution is **not** failed on a broken stream: the client retries (**reconnect attempts**, default 3) and the supervisor keeps the child running for the **reconnect grace** (default 60s) so a re-attach can resume. Only when the retries are exhausted (or the context was explicitly cancelled) does the execution fail and fall through to recovery. Env overrides: `ORCHICON_RECONNECT_ATTEMPTS`, `ORCHICON_RECONNECT_GRACE_SECONDS`.

#### Ask Orchicon
1. Navigate to the **Ask Orchicon** tab in the sidebar
2. Click **+ New Conversation** to start a new chat session
3. Orchicon can:
   - **Answer questions** about Orchicon, your projects, workers, work items, and workflows
   - **Create, read, update, and delete** projects, work items, workers, workflows, and other entities
   - **Create project directories** on the filesystem with optional scaffolding (`src/`, `docs/`, `tests/`)
   - **Diagnose failures** — ask "Why did the last workflow fail?" to get failure analysis
   - **Check usage and costs** — ask "How much have I spent?" for cost breakdowns
   - **View and update settings** — ask "Show my settings" or "Update my default model"
4. Chat history appears in the right sidebar — switch between or resume past conversations
5. The agent always asks clarifying questions before mutating data and refuses non-Orchicon requests

**How it works (proper MCP, not text emulation):** the chat's `opencode run` subprocess is launched with the built-in **Orchicon MCP server registered by default** in its config. `orchicon mcp` exposes the full Ask Orchicon tool registry over the Model Context Protocol (stdio JSON-RPC), tenant-scoped via `ORCHICON_MCP_TENANT_ID`. The model calls Orchicon tools natively as `orchicon_<tool>` (e.g. `orchicon_list_projects`, `orchicon_create_work_item`) through opencode's MCP integration — no string-protocol tool-call emulation. Each chat also receives an **enabled projects** context block (fresh per message) and an **About Orchicon** primer describing how the platform works. See §MCP & the Orchicon MCP Server.

#### MCP & the Orchicon MCP Server

The binary ships an **MCP server** subcommand (`orchicon mcp`) that exposes Orchicon's tool registry over the Model Context Protocol (JSON-RPC 2.0, newline-delimited stdio). It is consumed by opencode and any other MCP client (Claude Desktop, Cursor, …).

- **Discovery/execution**: `tools/list` returns every registered tool with a JSON-schema input; `tools/call` executes it (failures come back as `isError` results, per the MCP spec). The server echoes the client's MCP protocol version so the handshake succeeds with any client (opencode 1.18 sends `2025-11-25`).
- **Tenancy**: the server is scoped to one tenant per process via the `ORCHICON_MCP_TENANT_ID` env var, set by the control plane through the opencode config `environment` map of the injected MCP entry. Unset (e.g. a human wires `orchicon mcp` into Claude Desktop manually) → dev tenant with a warning.
- **Default registration**: `BuildConfigContent` (the `OPENCODE_CONFIG_CONTENT` injected into every opencode run) now registers the built-in Orchicon MCP by default — for **in-process worker executions** and **Ask Orchicon chat** (both co-located with the plane's Postgres). It is deliberately **not** registered for **runtime-container executions**: the per-workflow runtime container is an isolated, root-free sandbox with no network route to the plane's Postgres, and handing it the DB DSN would break the security model. The user's own opencode-config MCP servers are still merged in everywhere.

### Authentication

- **Local dev**: Built-in OIDC provider (HS256) — no external IdP needed
- **Production**: Real OIDC issuer with authorization-code flow
- **API keys**: SHA-256 hashed, least-privilege scopes for headless/CI clients
- **Frontend**: Access tokens in memory; refresh tokens in HttpOnly cookies

---

## Development Guide

### Local Development Loop

The fastest local development cycle — stop the dev instance, rebuild the binary + image, start it again:

```bash
make container-rebuild instance=dev     # stop dev container → build bin/orchicon + image → start dev container
```

Or run the individual steps:

```bash
make build                              # bin/orchicon (frontend + container configs embedded)
scripts/container.sh down dev           # stop the dev instance
scripts/container.sh up dev             # start it again with the new image
```

### Dual-Instance (dev + prod containers)

Orchicon can run two isolated single-container instances side by side: a **dev** instance (`orchicon-cnt-dev`, http://localhost:8080, Grafana http://localhost:3002) for daily development and a **prod** instance (`orchicon-cnt-prod`, http://localhost:8091, Grafana http://localhost:3003) for dogfooding. They share no ports, databases, or state — restarts to one never affect the other. `scripts/container.sh up dev|prod` manages each, reusing the compose-era Postgres volumes so data carries over. See §Single-Container Deployment.

### Single-Container Deployment

The entire Orchicon stack — Postgres, NATS, the Grafana telemetry plane (Tempo, Loki, VictoriaMetrics, OTel collector, Grafana), and the control plane — can run in **one container**. The `orchicon` binary is the PID-1 supervisor (`orchicon container`, `cmd/orchicon/container.go`):

- spawns children in dependency order (postgres → nats → telemetry → control plane), gating on readiness probes
- prefixes each child's stdout/stderr with its component name
- restarts crashed children with exponential backoff; forwards SIGTERM/SIGINT and waits for graceful exit
- writes the embedded Tempo/Loki/collector/Grafana configs into the data dir (`@DATA_DIR@` substituted)

**Build the image** (the binary must embed the built frontend — `make fe-build` first):

```bash
make build                                   # bin/orchicon (frontend embedded)
cp bin/orchicon deploy/container/
docker build -f deploy/container/Dockerfile -t orchicon:local deploy/container
```

**Run:**

```bash
docker run --rm -p 8080:8080 -p 3002:3000 \
  -v orchicon-data:/var/lib/orchicon \
  orchicon:local
```

Or use the lifecycle script (dev + prod dual-instance, data preserved):

```bash
scripts/container.sh build            # build the image (requires bin/orchicon)
scripts/container.sh up dev           # start the dev instance container
scripts/container.sh up prod          # start the prod instance container
scripts/container.sh status           # show both instances
scripts/container.sh logs dev         # tail the dev supervisor log
scripts/container.sh down dev         # stop + remove the dev instance
```

- **Data preservation**: the dev/prod instances reuse the compose-era Postgres volumes (`orchicon_postgres-data` / `orchicon-prod_postgres-data`) from the old Docker Compose workflow, so your existing data survives the switch to the single container. The container's postgres runs as the data dir's owner (uid 70 for the alpine-era volumes). The script refuses to start while the matching compose-era postgres is running (two postgres processes on one data dir corrupt it); start with an empty DB via `ORCHICON_PG_VOLUME=fresh`.
- Control plane: `http://localhost:8080` (API + UI + `/grafana`)
- Grafana: `http://localhost:3002` (embedded in the Telemetry page)
- **Worker executions**: the container ships the `opencode` runtime. **Mounts are scoped** — not the whole `$HOME`:
  - `~/.config/opencode` (read-only) + `~/.local/share/opencode` (rw) so workers use your real model providers.
  - **project dirs/files from a manifest**: the control plane writes `/var/lib/orchicon/project-mounts` (every `project_dir` + `context_files` from the projects table, refreshed every 30s). `container.sh up`/`rebuild` mounts each listed path at its host location. **After you save a project dir or context files in the UI, run `scripts/container.sh sync-mounts [dev|prod]`** to apply — Docker can't add bind mounts to a running container, so `sync-mounts` compares the manifest to the live container's mounts and recreates it when any are missing.
  - Extra paths: `ORCHICON_PROJECT_MOUNTS` (space-separated host paths).
  - **Ownership**: the control plane and its worker subprocesses run as **your host user** (`id -u`/`id -g` passed via `ORCHICON_HOST_UID/GID/HOME`), so files workers create in mounted project dirs are owned by you, not root. Infra processes keep their own users (postgres uid 70, telemetry root).
- Published image: `ghcr.io/beardedparrott/orchicon` (built + pushed by the release workflow on every version tag, tagged `vX.Y.Z` + `latest`).

**Environment variables** (all optional):

| Variable | Default | Purpose |
|---|---|---|
| `ORCHICON_TELEMETRY` | `embedded` | `none` skips the telemetry processes (≈96 MiB); `remote` skips them and exports OTLP to your own collector |
| `ORCHICON_DATA_DIR` | `/var/lib/orchicon` | Persistent state root (postgres, nats, telemetry data, configs) |
| `ORCHICON_GRAFANA_PUBLIC_URL` | `http://localhost:8080/grafana` | Grafana's public root_url (change when publishing on other ports) |
| `ORCHICON_*` | — | Any control-plane env var (DSNs, ports) overrides the container defaults |

**Measured footprint** (single container, this stack): full telemetry ≈ **384 MiB** resident; `ORCHICON_TELEMETRY=none` ≈ **96 MiB** — vs ~2.7 GB for the ClickHouse-era compose stack.

Dual-instance (dev + prod dogfooding) is two containers with offset published ports (`-p 8080:8080 -p 3002:3000` and `-p 8091:8080 -p 3003:3000`), separate data volumes.

### Workflow Runtime Containers

Worker executions run inside **one short-lived container per active workflow run** (Azure Pipelines self-hosted agent model). It is created when a run leaves `pending`, every execution for that workflow is dispatched into it, and it is killed when the run reaches a terminal state (`completed` / `failed` / `aborted`). Everything inside is ephemeral — installed tools, caches, and sessions are wiped on teardown, so each workflow starts from a pristine, fully-armed environment.

> **Platform note (Windows):** the entire runtime layer — the daemon, its POSIX unix socket, and the container mounts — is Linux/POSIX-only and is **not ported to native Windows**. On Windows the stack runs inside **WSL2** (see [§Installation Guide — Windows via WSL2](#installation-guide)): the WSL2 kernel is real Linux, so the runtime containers, the daemon, and the single-container stack work exactly as on Linux. The Windows installer (`scripts/install.ps1`) only provisions WSL2 and installs the Linux binary into the distro.
>
> **Orchicon MCP availability:** the built-in Orchicon MCP server is registered by default for **in-process** executions and Ask Orchicon chat, but **not** inside these runtime containers — the sandbox has no route to the plane's Postgres and is deliberately kept DB-credential-free (§MCP & the Orchicon MCP Server).

**Components:**

- **`orchicon runtime-daemon`** (host process): the only process with access to the Docker socket. Serves a narrow HTTP API over a unix socket (default `/tmp/orchicon-runtime/runtime.sock`, bind-mounted as a **directory** into the supervisor container at `/var/run/orchicon-runtime`): create/kill/list runtime containers, exec into them, signal a running exec, build/remove runtime images. The directory mount (not a single-file mount) means a daemon restart — which recreates the socket file — never staleness the supervisor container's socket. The daemon also runs an age-based orphan sweep (default: remove `orchicon-runtime-*` containers older than 24h; `ORCHICON_RUNTIME_MAX_AGE` / `ORCHICON_RUNTIME_SWEEP_INTERVAL`) as a hard backstop for containers leaked while the plane is down. Every request is validated — image allowlist (the base + `ORCHICON_RUNTIME_IMAGES` stock images + any locally-present image carrying the inherited `org.orchicon.runtime-base` label), mount sources restricted to the projects root, and an argv[0] allowlist (`opencode`, `orchicon`, `bash`, `sh`) — so the control plane can never create an arbitrary container. Started by `scripts/container.sh up`; manage with `scripts/container.sh runtime-daemon` / `runtime-stop`.
- **`orchicon runtime-supervisor`** (PID 1 inside each runtime container): listens on a unix socket (`/tmp/orchicon-agent.sock`), runs `opencode run` as a child, streams stdout/stderr back, and signals children by exec_id. Builds the execution-guard shim in-container so workers run under the same `rm`/`sudo`/`dd`/`mkfs` path-scoped safety guard as the in-process path.
- **`orchicon runtime-client`** (in-container): forwards a dispatch request from the daemon (via `docker exec`) to the supervisor socket and relays the streamed events back, so the daemon never needs shell-level access to the container.

**Lifecycle:** the `WorkflowReconciler` ensures a runtime container when a run leaves `pending` (mounting the project's `project_dir`) and reaps it when the run reaches terminal. A 30s adopt sweep in the control plane kills orphan containers (runs no longer active) and ensures containers for active runs — covering aborts, plane crashes, and externally-terminalized runs. **Instance-scoped**: every runtime container is labeled with its owning instance (`orchicon.instance=dev\|prod`), and each plane's adopt list/reap only sees its own containers — dev and prod share one runtime daemon but never reap each other's runtimes (the daemon's age-based orphan sweep is the global backstop). The same sweep runs an **execution-liveness reaper**: executions still `running` whose process is gone (plane restart, lost runtime container) are failed with `execution lost: control plane restarted or runtime container gone` and their work item transitions to failed, so the workflow's recovery step re-dispatches in a fresh runtime instead of the run getting stuck. The adapter **self-heals** on dispatch too: it ensures the runtime container exists before every execution so a recovery re-dispatch can't race ahead of the adopt sweep (container creation is serialized in the daemon so the reconciler's `EnsureForRun` and the adapter's self-heal `Create` never race `docker run` on the same name). Headless `orchicon serve` (no daemon socket) disables runtime containers, stays in-process, and still reaps in-process executions orphaned by a restart.

**Runtime adapter CLIs are mounted, never baked:** the images contain **no adapter binary**. The daemon mounts the operator's host `~/.opencode` install (read-only) into every runtime container and puts its `bin/` on PATH, so the supervisor can exec `opencode` — the same mount `container.sh`/`orchicon install` add to the main container for in-process dispatch. The supervisor's `argv[0]` allowlist (`runtimeBinAllowlist` in `internal/runtime/agent.go`) lists the adapter binaries Orchicon may exec — `opencode` today; `claude`/`codex` get one added entry when those adapters land. This is the licensing-safe pattern for all future adapters: **the product mounts the operator's own install; it never ships, downloads, or redistributes the CLI.**

**Security model — no root process in the runtime container:**

- The runtime container runs as the **host user's uid** (`ORCHICON_HOST_UID`, default 1000) with the image rootfs **chowned to that uid**, so workers have full write control over the ephemeral filesystem (they can install tools) while any bind-mounted project directory is written as the host user — never as root. A worker cannot `chown` a project file to root or escalate to the host.
- `dpkg` refuses to run as non-root, so system packages (python, node, build-essential, gh, …) are **baked at build time** (`deploy/runtime/Dockerfile`); runtime installs use user-space package managers (`pip` with `PIP_BREAK_SYSTEM_PACKAGES`, `npm`, `mise`, `uv`, `curl`) into the chowned rootfs / ephemeral `$HOME`.
- The daemon mounts `~/.config/opencode` and `~/.local/share/opencode` **read-only**; the supervisor redirects each worker's opencode state to an ephemeral `XDG_DATA_HOME` under `/tmp` (seeded with `auth.json`), so sessions/keys never touch the host's real opencode data. Git identity + credential store are mounted read-only (PR/merge workers need them).
- Per-runtime resource limits: 4 CPU / 4 GB memory / 2 GB tmpfs `/tmp` (configurable via `ORCHICON_RUNTIME_CPUS` / `_MEMORY` / `_TMPFS` on the daemon).

**Runtime images (self-service builds):** the image a workflow run's container uses is chosen **per work item** (`work_items.runtime_image`, backend-stamped to the base image when empty). The **Runtime Images** page (sidebar) lets you define and **build** custom images on the host runtime daemon: a structured form (apt packages, toolchain lines, env) with a **live Dockerfile preview** that doubles as an advanced raw-Dockerfile editor, plus a **Deploy** button that streams the `docker build` log. Editing a ready image reverts it to draft so it must be rebuilt; delete removes both the spec row and the local Docker image (gated on no active run using it). Every build is guaranteed to derive from the base image — the daemon rewrites the Dockerfile's `FROM` line to the base and injects the `org.orchicon.runtime-base=true` label, which is also the container-create gate (a locally-present image carrying that inherited label is accepted without a separate registration). The workflow-run start resolves the image (template → bound work item; one-shot → the WORK_ITEM markers' items, all must agree or the run fails at start) and stores it on the run, so a self-healed container is recreated with the identical image. Stock variants ship alongside the base: **`:gui`** (`deploy/runtime/Dockerfile.gui`, headless Qt/tkinter/X11 libs) and **`:dev`** (`deploy/runtime/Dockerfile.dev`, the dogfooding image — Go/Node/buf/atlas plus a baked PostgreSQL 15 for in-sandbox DB testing). Both double as reference templates for custom images.

**Dogfooding Orchicon (the `:dev` image):** to have a worker build and test the Orchicon repo itself, set the project's `project_dir` to your Orchicon checkout on the host and give its work items the `:dev` runtime image. The per-workflow runtime container mounts `project_dir` automatically at container-create time (`Lifecycle.EnsureForRun` reads it from the projects table — no `sync-mounts` needed for runtime containers; that script only applies to the long-lived single-container instance, where Docker can't add bind mounts to a running container). The checkout must be under the daemon's `AllowedRoots` (default `$HOME/projects`). Inside the sandbox a worker can run `go build`/`go vet`/`make gen`/`make fe-build` and the full `make ci` DB path by booting a **disposable in-sandbox Postgres** (non-root): `initdb` + `pg_ctl` into the ephemeral tmpfs, then `psql` to create the `orchicon` database — the RLS gate (`check-rls.sh`), migrations, and DB unit tests then run against `localhost:5432`. The instance dies with the container and never touches the plane's Postgres, preserving the no-DB-route sandbox invariant.

**Build:** `make container-build` also builds `orchicon-runtime:local` (plus `orchicon-runtime:local-gui` and `orchicon-runtime:orchicon-dev`). The release workflow ships the runtime image to GHCR (`ghcr.io/beardedparrott/orchicon-runtime:<version>` + `:latest`, plus the `:gui` and `:dev` variants) — the one-command install pulls it, and the runtime daemon defaults to that image (`ORCHICON_RUNTIME_IMAGE` overrides; local dev pins the locally-built tag). `ORCHICON_RUNTIME_IMAGES` adds extra allowlisted stock images (base always included). **Model note:** executions dispatch with the worker's pinned `model_ref`; verification workers should pin a free model (e.g. `opencode/deepseek-v4-flash-free`).

### Manual Development Setup

```bash
# Terminal 1: Full stack (single container)
scripts/container.sh up dev           # Postgres, NATS, OTel, Tempo, Loki, VM, Grafana + control plane

# Terminal 2: Migrations
make migrate                          # Apply database migrations

# Terminal 3: Frontend (optional, for hot-reload against the container's :8080)
make fe-install && make fe-dev        # Vite dev server on :5173
```

For source-level iteration on the control plane itself, rebuild the image and restart the instance (see the Local Development Loop above).

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
| **Container** | |
| `container-build` | Build `bin/orchicon` + the container image |
| `container-rebuild` | Stop an instance, rebuild the image, start it (usage: `make container-rebuild instance=dev\|prod`) |
| `container-up` | Start the dev single-container instance |
| `container-down` | Stop the dev single-container instance |
| `container-status` | Show single-container instance status |
| `container-logs` | Tail the dev container instance logs |
| `container-ps` | List orchicon container instances |
| **Frontend** | |
| `fe-install` | Install frontend dependencies |
| `fe-dev` | Start Vite dev server |
| `fe-build` | Build for production |
| `fe-lint` | Lint frontend |
| **Install** | |
| `install-dry-run` | Dry-run the install script (no changes made) |
| `install-uninstall` | Uninstall Orchicon via the install script |
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
2. **Container instance starts healthy** — `make container-build && scripts/container.sh up dev`, then `scripts/container.sh status` shows the dev instance `running (healthy)`; `curl http://localhost:8080/healthz` returns `{"status":"ok"}`
3. **Migrations apply cleanly** — on a fresh container data volume (`ORCHICON_PG_VOLUME=fresh`); `make rls-check` passes
4. **Control plane boots** — `make build && make run`, then `curl http://localhost:8080/healthz` returns `{"status":"ok"}` in <2s
5. **Frontend renders** — `make fe-dev`, then `curl http://localhost:5173/` returns HTTP 200

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
| `ORCHICON_GRAFANA_URL` | `http://localhost:3002` | Grafana UI URL (proxied same-origin under /grafana) |
| `ORCHICON_TEMPO_URL` | `http://localhost:3200` | Tempo query API URL |
| `ORCHICON_LOKI_URL` | `http://localhost:3100` | Loki query API URL |
| `ORCHICON_VM_URL` | `http://localhost:8428` | VictoriaMetrics query API URL |
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
| `ORCHICON_REAP_GRACE_SECONDS` | `60` | Liveness reaper: min execution age before reaping is considered (overrides DB setting) |
| `ORCHICON_REAP_CONSECUTIVE_FAILURES` | `3` | Liveness reaper: consecutive not-alive probes before an execution is reaped (overrides DB setting) |
| `ORCHICON_RECONNECT_ATTEMPTS` | `3` | Transport resilience: client retries of a broken exec stream (overrides DB setting) |
| `ORCHICON_RECONNECT_GRACE_SECONDS` | `60` | Transport resilience: supervisor keep-alive for an orphaned child before killing it (overrides DB setting) |

---

## Troubleshooting

### Container / Stack Won't Start

| Symptom | Likely Cause | Fix |
|---|---|---|
| Instance stays `unhealthy` | Corrupt/stale state | `scripts/container.sh down dev && scripts/container.sh up dev`, or start fresh with `ORCHICON_PG_VOLUME=fresh` |
| Grafana datasources missing | Embedded configs stale | Re-build the image so `deploy/container/configs/grafana-provisioning/` is re-embedded (`make container-build`) |
| Control plane won't connect to DB | Postgres not healthy yet | Wait for the PID-1 supervisor to bring postgres up; check `scripts/container.sh logs dev` and `ORCHICON_POSTGRES_DSN` |
| Postgres data-corruption guard blocks start | Compose-era postgres still owns the volume | Stop the old compose-era postgres container first, or use `ORCHICON_PG_VOLUME=fresh` |

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
| Grafana iframe blank | Sub-path config missing | Grafana must run with `GF_SERVER_SERVE_FROM_SUB_PATH=true` and `root_url=<plane>/grafana` — the control-plane `/grafana` proxy only strips the prefix (see `internal/api/api.go`) |
| React Error #310 (hook mismatch) | Conditional hook call | Ensure hooks are called unconditionally — check for early returns before hooks |
| Workflow canvas not loading | Missing `reactflow/dist/style.css` | Import React Flow CSS in the route component |

### Runtime Issues

| Symptom | Likely Cause | Fix |
|---|---|---|
| Worker execution never starts | `opencode` not in PATH | Install opencode CLI: `curl -fsSL https://opencode.ai/install | bash` |
| System prompt not sent to worker | Wrong env var | Must use `OPENCODE_CONFIG_CONTENT` with custom agent, not `OPENCODE_SYSTEM_PROMPT` |
| Loop decision stuck | Superseded step run conflict | Workflow reconciler must skip `SupersededBy != ""` runs |
| Stale decisions leaking across runs | Previous `_decision` file | Clear `.orchicon/<run_id>/` files between steps |
| Worker cannot delete or run destructive commands | Sandbox layers | Workers are intentionally sandboxed (see Architectural Pattern 8). Direct bash is blocked by opencode permission deny rules; subprocess/TUI-issued commands (e.g. `rm -rf /` inside a python TUI) are blocked by the OS-level execution guard (`internal/guard/guard.go`, built inside the workflow runtime container); and all canned workers' prompts carry the "Safety rules" block. Review/QA workers run `semgrep scan --config .orchicon/semgrep_orchicon.yml --error .` (Semgrep + Orchicon ruleset) to catch dangerous patterns before merge. |
| Worker wiped files outside the project | Execution guard bypassed via absolute path | The guard is defense-in-depth, not containment. A worker that invokes `/bin/rm` by absolute path or writes its own binary escapes it. Run Orchicon as the single-container deployment (§Single-Container Deployment) for a real process-isolation boundary. |

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
