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
| github.com/coreos/go-oidc/v3 | latest | OIDC relying-party verification (BYO IdP) |
| github.com/zitadel/oidc/v3 | v3.49.2 | Embedded OpenID Provider (`internal/auth/op`, Apache-2.0) |
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
        Reconcilers[Reconciler Manager<br/>Task / Workflow / Recovery / ScheduledRun / Sequence / RecurringFire]
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

1. **Kubernetes-style Reconcilers** — Six reconcilers (Task, Workflow, Recovery, ScheduledRun, Sequence, RecurringFire) run in a shared manager with per-kind PostgreSQL advisory locks for leader election. Each has a work queue with exponential backoff and a scan pass for discovering work. The work-queue `dequeue` is bounded to one rotation pass (a single not-ready key returns `ok=false` instead of busy-looping — a field incident pinned a core at ~150% CPU and froze the reconciler), and the workflow DAG-progression loop is capped (`maxDAGPasses`) so a pathological run can never wedge a reconcile goroutine. A step-dispatch failure that can't be resolved (e.g. a missing/corrupted worker-version lookup) fails that **step** rather than erroring the whole pass, so completed upstream steps are not rolled back with it.

2. **Transactional Outbox** — Every mutation writes an outbox row in the same database transaction as the state change. A background relay polls unpublished rows every 500ms and publishes to NATS JetStream for at-least-once delivery.

3. **Single Binary** — The Go binary embeds the single-container runtime configs (`deploy/container/configs/`), SQL migrations, and the built frontend SPA via `go:embed`. No external dependencies at runtime beyond Docker. The **single container** (`orchicon container` runs the whole stack as PID-1 — §Single-Container Deployment) is the only full-stack deployment; the same binary also runs headless via `orchicon serve`.

4. **Non-blocking OTel** — The OpenTelemetry pipeline uses `grpc.NewClient` (non-blocking dial), so the control plane boots in <2 seconds even when the OTel collector is not yet healthy.

5. **Connect-ES** — Single Protobuf schema generates both Go server and TypeScript client code. Supports unary RPC, server-streaming, and client-streaming over the same interface.

6. **RLS-backed Tenant Isolation** — Every tenant-scoped table has a PostgreSQL Row-Level Security policy as a backstop. The data-access layer also injects `app.tenant_id` via session variables.

7. **Adapter Bridge Pattern** — Runtimes are pluggable gRPC sidecars. The built-in adapter drives the OpenCode CLI — locally as a subprocess (headless `orchicon serve`) or, for workflow-run executions, inside the per-workflow runtime container via `orchicon runtime-daemon` — parsing its JSON telemetry output. Future runtimes implement the `orchicon.adapter.v1` gRPC contract.

8. **Worker Sandboxing (layered defense)** — Every worker execution is contained by three layers, all applied to **every** worker automatically and enforced even under `--auto`:
    - **opencode permission deny rules** (`permissionRules()` in `internal/opencode/config.go`) injected via `OPENCODE_CONFIG_CONTENT`. `external_directory` is `deny` by default with a **single precise carve-out**: `/tmp/orchicon/**` is `allow`, so workers can use `/tmp/orchicon` as a scratch directory (screenshots, logs, downloaded artifacts) but every other path outside the project's `--dir` is blocked — the carve-out deliberately does not match the supervisor socket, the execution-guard shims, or the `/tmp/opencode-data-*` dirs that hold the seeded model auth.json copies. An extensive `bash` deny list blocks `rm`/`sudo`/`dd`/`mkfs*`/`fdisk`/`parted`/`shred`/`wipefs`/LVM tools, root-wide `chmod -R`/`chown -R`, `/dev/sd*` redirection, shell-construct smuggling variants (`(rm -rf /) &`, `{ rm -rf /; }`, chained `;`/`&&`/`&`/`|`), and download-and-execute. No catch-all `*` allow rule is emitted.
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
        string status "pending|ready|assigned|running|checkpointing|succeeded|failed|cancelled|recovering|scheduled|recurring"
        string assigned_worker_ref
        json recurring_schedule "NULL or {frequency, interval, days[], start_date, start_time}"
        timestamp next_run_at "computed next occurrence of a recurring item"
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
│   ├── scheduler/               # TaskReconciler, WorkflowReconciler, ScheduledRunReconciler, SequenceReconciler, RecurringFireReconciler
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
│       ├── index.css             # Global styles + 28 themes (Tailwind + CSS vars)
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
│           ├── schedules.tsx     # Upcoming/History view of scheduled work items
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
| **Recurring schedules fire loop** | `internal/scheduler/recurring_fire_reconciler.go` (due scan + fire + `next_run_at` advance) |
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
| `orchicon runtime-supervisor` | Runtime container PID 1 (streams `opencode run`; hosts the container's opencode serve) |
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
3. Set the structured prompt fields **Role, Skills, Behavior, AGENTS.md** (the editable source of truth; the server composes them into the `system_prompt` the model receives). Saving a draft round-trips these fields — they persist exactly as entered.
4. Publish the worker (draft → published)
5. Workers are versioned; published versions are immutable

**Canned workers** are pre-seeded in the dev tenant and available immediately:

| Worker | Purpose |
|--------|---------|
| Senior Software Engineer | Full-stack development, implements features and fixes bugs |
| PR Reviewer | Code review — finds bugs, security issues, and correctness problems |
| QA Engineer | Functional and regression testing — validates acceptance criteria |
| DevOps Engineer | Repository setup (early steps) and PR/merge after approval (late steps) |
| Design Approver | Worker-backed approval of the architecture/design plan — reviews the plan against acceptance criteria, approve/reject |
| Code Approver | Worker-backed approval of the completed implementation — verifies done-ness after QA/PR, approve/reject |
| Principal Software Architect | Architecture design, ADR documentation, and technical strategy |
| Senior Software Engineer - Vision | Copy of Senior Software Engineer on a vision-capable model — full-stack development plus visual verification of UI work |
| Principal Software Architect - Vision | Copy of Principal Software Architect on a vision-capable model — architecture design plus visual inspection of UI prototypes |
| QA Engineer - Vision | Copy of QA Engineer on a vision-capable model — functional/regression testing plus visual & accessibility verification of the UI |

The Vision workers (identified by the `- Vision` suffix) are the replacements for the retired UI-canned workers (`w_ui_developer`, `w_ui_design_architect`, `w_ui_qa_engineer`). They are exact copies of their non-UI counterparts with three additions: the vision-capable `opencode-go/mimo-v2.5` default model, extra UI/design-system/accessibility/responsive skills, and the **Browser automation (Playwright) — VISUAL verification** block in their AGENTS.md: the runtime container has no root process, so headless Chromium must launch with `--no-sandbox` (via a project `scripts/browser.cjs` helper with `launch()`/`shot()`). The `:orchicon-dev` runtime image preinstalls Playwright + Chromium (`PLAYWRIGHT_BROWSERS_PATH=/ms-playwright`, `NODE_PATH=/usr/local/lib/node_modules` so `require('playwright')` resolves globally). The protocol: start the app in-container, launch the browser, navigate at desktop + mobile viewports, screenshot to `/tmp/orchicon/`, **read the screenshot back** (that is how the model sees the UI), and iterate against the acceptance criteria. The seeder stops seeding the retired UI workers and deletes any still-seed-managed instance on boot.

The worker identity (Role, Skills, Behavior, AGENTS.md) is included in every dispatch prompt. Workers also receive workflow-aware context: step position, iteration count, execution history, and prior issues found.

Every dispatch prompt is also prefixed with a fixed **worker identity preamble** (built by both composite builders, `internal/scheduler/reconciler.go`): *"You are an autonomous worker running inside the Orchicon orchestration platform. You are not a human operator and there is no human attached to this run. You execute one assigned work item per run, operate within your role and the project's acceptance criteria, and report your result via the `ORCHICON WORKER SUMMARY` contract. Work autonomously to completion; do not wait for interactive approval for work that is within your assigned scope."* The canned workers also carry the same identity sentence at the start of their seeded Role (`cannedWorkerIdentity` in `internal/db/seed_workers.go`), so the stored worker row and the dispatched prompt agree. This is the mechanism that distinguishes an **in-Orchicon worker** (autonomous: PRs into `develop`, merges on approval, reports via the summary contract) from a **human-facing session** (an agent in a chat that must ask before PRing/merging).

Worker output is parsed for the standard `ORCHICON WORKER SUMMARY: success|failure — <summary>` marker, which routes the workflow to the next step or triggers a loop-back. **The summary word is the single decision signal** — there is deliberately no separate `_decision:` or `_issues:` channel that can override it. A standalone `_decision:` line in output is ignored; an `_issues:` block is captured for the run view and `.orchicon/<run_id>/issues` but never affects routing. This removes the class of false failures where a worker's prose (e.g. a "non-blocking, not `_issues:`" nitpick list) was misparsed as an issues block and forced a loop-back despite a `success` summary.

An execution is only reported `succeeded` when the run completes with the final model step fully delivered (`step_finish` received). opencode's `--format json` emits the entire model response as ONE stdout line, and a scanner cap smaller than the response used to drop that line **and** every event after it (a `bufio.Scanner` is permanently broken after `ErrTooLong`) — so a large final answer made an otherwise-successful execution come back `succeeded` with **empty output**, and the loop_decision step saw no `_decision` signal and re-asked until it failed. Two fixes close this: the runtime path now uses the same 1MB line cap as the local path (was 64KB), and the adapter tracks `step_start` vs `step_finish` counts so a clean exit with an unpaired `step_start` is downgraded to a failure (`execution ended before the final model step completed (model response stream truncated or event dropped)`) instead of a silent success. The same single-signal contract now also covers the **re-ask budget** of a `loop_decision` step: it counts only genuine re-asks (step runs named `"Reviewer (re-ask)"` created for a MISSING decision signal), never a reviewer's ordinary loop iterations — a reviewer that legitimately looped back via explicit `_decision: failure` results (or was accepted) has consumed none of the budget, so a truncated final turn with no signal gets a real re-ask instead of failing on a pre-spent `max_reask`.

#### Creating Work Items
1. Navigate to **Work Items** → **New Work Item**
2. Select a Project and Work Item kind (Epic → Feature → Task → Subtask)
3. Add description, acceptance criteria, and assign a worker
4. Work items form a DAG with dependency edges (cycle detection enforced)

**Parent / hierarchy editing:** the work item detail page (`/work-items/$id`) shows a **Parent** card for any child — the parent's kind pill + title (linked) in view mode, and a searchable parent picker in edit mode. The picker (`WorkItemParentSelect`, ADR-WIT-5) filters candidates client-side as you type and renders each option's kind as a color-coded badge; candidates are the same project's items at a strictly higher level (epic > feature > task > subtask); only epics are top-level, so a child cannot be un-parented and a cross-project move requires a parent in the target project. The same rules are enforced server-side on `CreateWorkItem`/`UpdateWorkItem` (shared `workitem.ValidateParent`) and by the Ask Orchicon `update_work_item` tool.

**Switching a work item's kind:** the edit page's **Type** control switches an item to any hierarchy kind (ADR-WIT-1/2). The server resolves the parent/child tree automatically inside the same transaction (`workitem.ResolveKindSwitch`, shared by the Connect handler and the Ask Orchicon `update_work_item` tool — `kind` param):

- **Parent** — switching to an Epic clears the parent; any other kind keeps the current parent when it is strictly shallower, otherwise the parent walks up the ancestor chain to the nearest shallower ancestor; an Epic switched to a non-Epic must pick a parent explicitly (the UI forces the choice). When a kind switch is combined with a project move in the same request, the same rule as a plain cross-project move applies: the parent must be chosen explicitly in the target project (naming the item's current parent only counts when that parent already lives in the target project, and the walk-up never crosses projects).
- **Children** — direct children that can no longer sit under the switched item (e.g. a Subtask under a Task switched to Subtask) move under the item's resolved parent (they become siblings).
- **Schedulability** — switching to an Epic/Feature clears the worker binding and scheduled start and demotes `ready`/`assigned`/`scheduled` to `pending`, so a re-typed item can never be dispatched by the TaskReconciler.
- **Guards** — a running/checkpointing/recovering item or one with an active workflow run is rejected (`FailedPrecondition`).

The UI confirms the consequences before saving ("N child items will move under the parent", "Worker assignment and scheduled start will be cleared"). The switch emits a `work_item.kind_changed` outbox event carrying `old_kind` + `new_kind` (plus `work_item.updated` for every reparented child), all in the same transaction. No migration is required — the `kind` CHECK constraint already exists.

**Auto-start is opt-in:** the edit page's **Scheduled start** card (shown for workflow-bound items) has a "Start immediately on save" checkbox that always opens **unchecked** — including legacy items whose stored `auto_start_workflow` is `true` — so saving an edit (a kind switch or any other change) never starts a workflow run unless the user explicitly checks the box. New items default `auto_start_workflow = false` (server default + the `work_items` column default, migration `20260807120000_work_item_auto_start_default_false.sql`). The server also refuses to auto-start on any kind switch unless `autoStartWorkflow=true` is sent in the same request. Scheduled runs are unaffected: `ScheduledRunReconciler` fires on `scheduled_start_at IS NOT NULL`, independent of `auto_start_workflow`.

**Work item context files:** a work item can carry its **own** `context_files` — absolute file **or directory** paths — exactly like a project. The new/create + edit pages show a FileBrowser card ("Work Item Context Files") bound to the item's project directory; check files and/or whole directories. When a worker is dispatched for the item, these paths are rendered into the composite prompt (`# Work item context` section) by the same shared renderer as project context: **files** are inlined (capped at 256 KiB, with a truncation note beyond that), **directories** are expanded into a bounded listing (up to 1000 entries, skipping VCS/build noise) with an explicit instruction to read every file and **not** open the directory path as a file ("not a file" error). Relative paths are resolved against the project directory (backward compat). The paths are read-only input — never mutated by the reconcilers — and are also mounted into the single-container instance (mount manifest) and per-workflow runtime containers (when outside `project_dir`). `UpdateWorkItem` clears the selection when sent an empty `context_files`; `CreateWorkItem`/`UpdateWorkItem` validate every path (absolute, no `..`, length caps) via the shared `internal/contextfiles` package. **Every context path (project and work item) must live inside the project's `project_dir`** — the only directory guaranteed to be mounted into the containers where workers run, so a path outside it would be invisible to the worker. `contextfiles.ValidateWithin` enforces this at the API boundary (both the Connect handlers and the Ask Orchicon tools), and the FileBrowser's checkbox tree is rooted at `project_dir` so the UI can't offer out-of-project paths in the first place. The Ask Orchicon `create_work_item`/`update_work_item` tools expose the same `context_files` field.

#### Work Items page (tree + kanban board)
The **Work Items** list page has two views sharing one filter bar, selection set, and auto-refresh loop:

- **Tree** — the Epic → Feature → Task → Subtask hierarchy with cascade (subtree) selection, tri-state parent checkboxes, indent guides, and file-explorer auto-expand when filters are active (ancestors of matches are shown so filtered results stay reachable). Rows surface **blocked** state from the dependency DAG (chain chip + tooltip listing the blocking items).
- **Board** — a Jira-style kanban with one column per server status (Pending/Ready/Assigned/Running/Succeeded/Failed/Cancelled; checkpointing/recovering render in Running, scheduled/recurring in Pending). Cards drag & drop between columns via dnd-kit (`@dnd-kit/core` + `sortable` + `utilities`); drops are **server-confirmed** (no optimistic transitions) with a transient "moving…" state and toasts. Two advisory gates reject obviously-wrong moves before the mutation: a **blocked** item cannot be dropped on Ready, and Epics/Features accept only pending/succeeded/cancelled. A per-card **"Move to…"** menu performs the identical mutation for keyboard/touch users.
- **Expand / Collapse all** — the filter bar carries an Expand all / Collapse all pair (ADR-WIT-4) that acts on every row with children in the currently loaded project, in both views. Each view stores its state in a different persisted set (board collapsed set, tree expanded set when unfiltered, tree collapsed set while a filter is active), so the buttons stay consistent with the per-item chevrons and survive reload.
- **Filtering** — search, kind, and status all compose **client-side** over the full fetched set (pageSize 1000); only sort goes server-side. (Search is client-side deliberately: a server-side search returns only the matching rows, orphaning a searched task from its epic and emptying the tree.) Select-all is tri-state over the visible filtered set — it selects only the items that pass the filters, never the dimmed ancestor container rows — and the selection clears on any filter change.
 - **Drag-to-reorder (tree)** — siblings under a parent can be dragged into a new **chain order** (sequential multi-workflow runs, below). The drop calls `ReorderWorkItems` → `sort_order` is renumbered for the sibling group in ONE server-side transaction. The post-drag click is suppressed so the drop never navigates into the item's detail page, and `project_id` is derived from the items being reordered, so drag works in the **All projects** view too. The filter bar's sort/filter dropdowns never call the RPC — they only change display order. Because the sequence cursor is derived from `sort_order` at reconcile time, a mid-sequence drag shifts only *future* arming.
 - **Position badges (board + tree)** — cards/rows are ordered by `sort_order` (then created_at) by default; a **`#N` chain-order badge** on every child of a parent keeps the true sequence order unambiguous under any non-default display sort (title/priority/created_at). The badge is derived from `sort_order` rank within `parent_id`, never from display order, and shows for any parent-with-children — even one with a stale workflow binding.
- **Auto refresh** — the list and dependency-graph queries poll every 5s, pause while the tab is hidden, refetch on window focus, and a `Live HH:MM:SS` indicator in the header makes the refresh visible.

Shared presentation lives in `frontend/src/components/work-items/` (meta, badges, card, tree, board, filter bar, selection hook, dependency utils); the route file is a thin shell. Schedules and the work-item detail page import the same theme-safe `KindBadge`/`KindPill`/`StatusPill` (the old hardcoded light-only colors are gone).

**Status while bound to a workflow run:** a work item that kicks off (or is bound to) a workflow run is a **shared input reference** — every step reads the same ticket (title, description, acceptance criteria, upstream context) and produces its own execution and output. `StartWorkflow` moves it to `running`, and it stays `running` for the whole run; it is never mutated per-step (no `assigned_worker_ref`, `workflow_step_id`, or prompt writes, no `ready`/`assigned`/`recovering` flips). The item reaches `succeeded`/`failed` only when the whole run completes/fails. Because the ticket is never written per-step, **two steps bound to the same ticket can run in parallel** — each step run owns its own execution (`worker_execution_id`) and its own results. When the run ends, the ticket's `results` carry a run-level narrative (`_run_narrative`) aggregating each step's summary/decision/issues plus every recovery episode.

**Acceptance Review:** the ticket also gains a first-class **Acceptance Review** field (`acceptance_review`, markdown) written in the same transaction that flips the item to `succeeded`/`failed`. It is a deterministic, human-readable aggregation of the run's own step results — the per-step `_summary`/`_decision`/`_issues` and any recovery episodes (the same data `_run_narrative` reads) — rendered as "What was delivered" (and, on a failed run, "Not delivered / needs attention"). It is *not* an LLM summarizer call: the reconciler runs on the critical path in a hot loop, and the step summaries are already the workers' own account of the work done (invariant #7 — no implicit model selection). The field is bounded (1 MiB), editable by humans via `UpdateWorkItem` (auto-generated reviews can be corrected), and empty until a bound run completes (standalone dispatches never populate it).

#### Viewing Schedules
1. Navigate to **Schedules** in the sidebar
2. The **Upcoming** view (default) lists scheduled work items (`status = scheduled`) **and** recurring work items (`status = recurring`) in chronological order with their next runtimes, grouped by local day (Today / Tomorrow / weekday date). Recurring items use `next_run_at` (the computed next occurrence) as their effective fire time, so the next occurrence drives their position in the agenda. Below the scheduled agenda, a **Queued** section lists the not-yet-armed children of a running sequence parent (pending items derived from the full project list — sequence children carry no `scheduled_start_at` of their own, only the parent is scheduled), ordered by chain order
3. Each card links to the work item (`/work-items/$id`) and its bound workflow (`/workflows/$id`); a right-aligned frequency slot shows **One-time** for single-run items and the recurrence description (e.g. "daily", "every 2 days") for recurring items. At rest a recurring item's fuchsia **recurring** status pill is the single recurrence signal; while an occurrence runs (status `running`) a fuchsia **Recurring** badge keeps the item visibly distinct
4. The **Running** view (toggle or `?view=running`) shows **any currently running workflow**: work items whose bound workflow run is in flight (status `RUNNING` / `CHECKPOINTING` / `RECOVERING` with a `workflow_run_id`), whether or not they carried a scheduled start — a workflow started manually, or via "Start immediately on save", shows here too. **Sequence parents** (active items with children and no bound workflow) also appear, labeled with a color-coded **multi-workflow** chip; their children carry the same chip plus a `#N` chain-order badge. The shown start time falls back to the item's `updated_at`/`created_at` when there is no schedule
5. The **History** view (toggle or `?view=history`) shows items that previously ran a workflow, most recent first, with links to the workflow run when one exists: items that had a scheduled start and reached a terminal status, **plus** terminal items that carried a `workflow_run_id` (completed sequence children, and single workflow runs started without a schedule), **plus** terminal sequence parents. The ran time falls back to `updated_at`/`created_at` when there is no schedule
6. The standard filter bar applies: search, project, kind (schedulable kinds only), and run-time sort order; bulk actions cancel running schedules, cancel upcoming schedules, or hard-delete history items
7. A live clock and countdown chips are driven by a single page-level timer (paused while the tab is hidden)

**Saving a schedule on a work item** flips that item's status to `scheduled` — setting `scheduled_start_at` in the work-item edit form switches it to `scheduled` no matter its current status (so it appears in Upcoming and fires via `ScheduledRunReconciler`). The flip is scoped to the edited item only, never a bulk change, and is skipped while the item is `running`/`checkpointing`/`recovering` (an in-flight run must not be re-armed) or when the same edit switches it to a non-schedulable kind (which clears the schedule).

**Recurring schedules** — instead of a one-time scheduled start, a work item can carry a **recurrence pattern** and re-fire on a schedule. Setting a `recurring_schedule` on create/update flips the item's status to `recurring` (mirroring the `scheduled` flip for `scheduled_start_at`); the item appears in Schedules → Upcoming (with `next_run_at` as its effective fire time, driving its position in the agenda) and fires via the `RecurringFireReconciler` (`internal/scheduler/recurring_fire_reconciler.go`). The pattern is defined by five fields:

- `frequency` — `minute | hourly | daily | weekly | monthly`
- `interval` — how many frequency periods between occurrences (`>= 1`, e.g. every 2 hours)
- `days` — a subset of `Mon, Tue, Wed, Thu, Fri, Sat, Sun` (weekly cadence; empty = every day)
- `start_date` — `YYYY-MM-DD` anchor date of the first occurrence
- `start_time` — `HH:MM` time of day occurrences fire

These are stored as JSONB on `work_items.recurring_schedule` (NULL = not recurring) alongside `next_run_at` — the computed next occurrence that doubles as the due-scan cursor (a partial index on `(tenant_id, next_run_at) WHERE next_run_at IS NOT NULL` keeps the scan small; migration `20260812040000_work_items_recurring_schedule.sql`). The edit page's **Recurring schedule** card and the Schedules card's recurrence slot (e.g. "daily", "every 2 days") render the pattern via `formatRecurrence`. Behavior:

- **Firing** — when `next_run_at` enters the due window (a 5-minute lookback covering reconcile-loop jitter), the reconciler fires the item **immediately** and **advances `next_run_at` to the next occurrence in the same pass** (optimistic version locking makes the fire idempotent per due window). A leaf fires its bound workflow via `StartWorkflowDirect`; a **parent with children** fires through the sequence engine (`StartSequence`, so its children run **sequentially**, one after another). **No new items are spawned** — the same item (or subtree) is re-armed each occurrence, never cloned.
- **Leaving recurring clears the schedule** — switching status to anything other than `recurring`, or sending an empty `recurring_schedule` (proto clear semantics), clears the pattern and `next_run_at` and demotes the item to `pending`. The edit page's Status dropdown clears the recurring-schedule card when you switch away from `recurring`.
- **Completion returns the item to `recurring`** — a recurring item **never goes terminal on a completed occurrence**: after its bound run or sequence cycle completes — success **or** failure — it returns to `recurring` (never `succeeded`/`failed`) with schedule and `next_run_at` intact (the cursor is recomputed from the schedule if it was cleared mid-cycle). A **failure of one occurrence does not stop future cycles** — the failed occurrence is recoverable through the normal per-step recovery flow, and the next occurrence still fires on schedule. See "Recurring items" under Sequential multi-workflow runs for the sequence-parent case.

**Sequential multi-workflow runs** — a parent work item (epic/feature/task) **with children** can be scheduled (or run-instantly) to run its children **one after another, depth-first**. The parent *is* the sequence run (no separate entity): its own status plus its children's statuses fully describe state, and "who's next" is **derived, never stored** — every reconcile pass recomputes the first direct child in `sort_order` whose status is not terminal-success, so a crash/restart mid-chain resumes correctly. Children keep their **own** bound workflows and runtime images (no config copy). **"Has children" is the sequence determinant**: a parent with children is a sequence run regardless of whether it still carries a workflow binding (a stale binding from before the item became a parent is ignored at fire time — the routing branches on children, never on the parent's `workflow_id`).

 - **Firing** (past-due schedule or run-instant): the parent → `running`, its stale workflow binding (if any) is **cleared**, **every descendant resets to `pending`** (prior successes from earlier manual runs included), and the first child in `sort_order` arms — it flips **directly to `running`** and its own bound workflow starts (no `ready`/`assigned`/`scheduled` detour).
- **Advance / completion**: a child reaching `succeeded` arms the next non-succeeded sibling; when all children are `succeeded` the parent → `succeeded`.
- **Failure halts**: a child failing → that child `failed`, every later sibling stays `pending`, the parent (and each container up the chain) → `failed`; nothing after the failure ever arms on its own.
- **Recurring items** never go terminal on a completed occurrence. When a recurring item's bound run or sequence cycle completes — success **or** failure — the item returns to `recurring` (never `succeeded`/`failed`) with its `recurring_schedule` and `next_run_at` intact (the cursor is recomputed from the schedule if it was cleared mid-cycle), so the `RecurringFireReconciler` scans and fires the **next** occurrence on schedule. A failed occurrence is recoverable through the existing per-step recovery flow; for a recurring sequence parent a failed child halts the **current** chain (later siblings stay `pending`, next cycle re-fires the subtree fresh). This applies to the workflow-reconciler run-completion success/failure paths, `failRunAtStart` (structural run failures — unresolvable runtime image, empty step DAG, missing workflow version), and the sequence-reconciler completion + failure-chain paths. A recurring item with no children is never a sequence parent; only non-recurring terminal children advance/halt a parent's chain.
- **Retry auto-resumes**: fix + retry the failed child to success and — because the cursor is derived — the chain continues with the next sibling automatically (the parent revives to `running` when its children are no longer halted). No manual "resume sequence" action exists.
- **Dependency gate**: an on-deck child whose external blockers aren't satisfied **parks** the chain on that child (parent `running`, child `pending`) until the blockers succeed, then it advances without human action. Only a human may fail a blocked child.
- **Recursion**: arming a container child starts its own nested sequence (its descendants reset to `pending`); a leaf failure fails the whole ancestor chain.
 - **Schedule-time validation**: scheduling or run-instantly runs a **full-subtree walk** and rejects outright if any child that must execute has no workflow bound (message names the offenders: `Cannot schedule "X": 2 children have no workflow set — "A", "C". Bind workflows or remove them from the sequence.`), if any bound workflow is not published/deprecated, if a bound workflow's current version has an **empty step DAG** (a run on it could never progress — the reconciler fails it at start), or if any worker-assigned (one-shot) child exists anywhere in the subtree (one-shots remain standalone-only). A workflow-less **leaf** (no children, no workflow) is also rejected at schedule time (`Cannot schedule "X": no workflow is set, so there is nothing to run.`) instead of silently storing a schedule that would never fire. Nothing starts on rejection. The same validation runs in the Ask Orchicon `schedule_work_item` / `update_work_item` tools. The edit form shows the schedule / run-immediately card for any item **with children** even when no workflow is selected (labeled as a sequence run), and a parent's own workflow binding is annotated as ignored — so a parent needs no workflow of its own to schedule.
- **Engine**: the `SequenceReconciler` (`internal/scheduler/sequence_reconciler.go`) scans sequence parents (`running`/`failed` + children + no `workflow_run_id`) every pass; `ScheduledRunReconciler`'s fire query now admits parents with children (branching on a **NULL** `workflow_id` — scanned as a nullable pointer so a sequence parent's missing workflow never crashes the scan or suppresses co-due bound schedules), and `WorkflowReconciler` notifies the sequence engine when a bound child reaches a terminal state so the chain advances immediately. Execution is **strictly one-at-a-time even across a mid-run drag**: before arming the on-deck pending child, the engine verifies no sibling is still in flight or halted (`running`/`checkpointing`/`recovering`/`assigned`/`ready`/`failed`/`cancelled`) — a `ReorderWorkItems` that sorts a pending sibling ahead of an in-flight child only shifts *future* arming, never starts a second child concurrently, and never skips an unfixed failure.
- **Manual control (START / RESUME / STOP)**: a sequence parent can also be driven explicitly through the `ControlSequence` RPC (and the Ask Orchicon `control_sequence` tool) when the derived cursor cannot act on its own. **START** re-fires the chain from child #1 (destructive — every descendant resets to pending; enabled when not actively sequencing, confirm-gated in the UI). **RESUME** continues from the first non-succeeded child, keeping prior results — the manual counterpart to the auto-resume path, needed because a *parked* parent (status `pending`, e.g. after STOP) is never picked up by the scan; enabled when the chain is halted (parent `failed`) or parked (parent `pending` with children). **STOP** parks the chain — parent → `pending` and `scheduled_start_at` cleared, nothing else — so children can be run standalone; an in-flight child finishes naturally because the engine only advances `running`/`failed` parents. All actions are rejected for non-sequence parents (leaf, or a bound-run ticket), and server-side status gates mirror the UI's display-only gating.
- **Schedules surface**: sequence parents appear in Upcoming (scheduled), Running, and History; their not-yet-armed children appear under a **Queued** section in Upcoming (pending children have no `scheduled_start_at` — only the parent is scheduled — so they are derived from the full project list, never a `SCHEDULED` query), and completed children / single runs without a schedule appear in History via their `workflow_run_id`. The Running view's membership predicate is extended to include active sequence parents (they have no single `workflow_run_id`). While firing the parent card shows a color-coded **`multi-workflow`** chip; children show the same chip plus a **`#N` chain-order badge**.
- **Sorting**: a new nullable `sort_order` column (scoped within `parent_id`, backfilled by `created_at`, indexed `(project_id, parent_id, sort_order)`) is the only mutation target of `ReorderWorkItems` (explicit drag). `ListWorkItems` with no `sort_by` returns `ORDER BY sort_order NULLS LAST, created_at` (cursor pagination keeps the stable id order), so tree/board show chain order by default.

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
2. Select an approver worker (e.g. Design Approver for an architecture/design plan, Code Approver for a completed implementation — each has a fixed review contract, no runtime role-guessing)
3. The step dispatches the approver worker like a task step, against the run's **shared work item** (the ticket from an upstream WORK_ITEM marker or the run's bound item) — no per-step approval work item is ever created; the **step run itself is the approval record**, carrying the composite prompt, the approver worker pin, the upstream review context, and the decision. Work Items stay clean — no "Approval: …" clutter rows.
4. The worker's `ORCHICON WORKER SUMMARY` output determines the decision:
   - `success` → approved, workflow proceeds
   - `failure` → rejected, workflow loops back (if loop_branch configured)
5. The decision is visible in the Approvals list alongside human reviews
6. On failure/retry the same ticket is re-dispatched (attempt counter incremented, still no work item created); a worker-backed approval step on a one-shot run with no bound ticket and no upstream WORK_ITEM marker fails with a clear message (same constraint as TASK steps)

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

**Facts learned ledger:**
A run carries a facts ledger so later steps inherit established facts instead of re-deriving them. Each worker records facts it established (root causes, environment gotchas, decisions) as `FACTS LEARNED:` lines inside its `ORCHICON WORKER SUMMARY`. The composite prompt aggregates every recorded fact into a `## Facts learned (this run)` section near the top and instructs workers to read `.orchicon/<run_id>/facts_learned` first (the `.orchicon/` file is also appended per step by the TaskReconciler). A fact already recorded is treated as established — workers are told not to re-verify or re-derive it, and to append a correcting `FACTS LEARNED:` line rather than re-investigate. This directly counters the observed over-verification pattern in review/QA steps (re-confirming the same environmental conditions 3–4× per session).

**Fan-in loop decisions (parallel review/QA):**
A `loop_decision` step may depend on **multiple** upstream steps. The gate waits until **all** upstreams are terminal, then aggregates their decisions: failure is decisive — if ANY upstream failed (or reported `_decision: failure`) the whole chain loops back to `loop_branch`; otherwise it proceeds forward only when every upstream succeeded. The non-human coding templates use this to run PR Reviewer and QA Engineer **in parallel** after the implementation step and fan both into a single gate before the Code Approver — the approval step then receives both review and QA results in its execution history, and a rejection by either loops the implementer's step and re-runs both. On re-entry both upstream steps are re-created (the chain between `loop_branch` and the gate), so QA always validates the final code state.

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
6. **Advisory stalls do not fail executions.** The stall monitor flags `no_file_progress` when a worker goes the `no_file_diff` window without touching files (default 15m). Because reviewers/analysts may legitimately produce output for long stretches without writing files, an advisory stall never kills the subprocess and never fails the execution — it sets a non-terminal `stalled` health notice, keeps the execution `running`, and revives it to `healthy` when file progress resumes (`OnRecovered`). Only genuine hang/loop signals (`no_progress`, `text_loop`, `repetition`) hard-kill and route to recovery.

#### Policy Management
1. Navigate to **Policies** → **New Policy**
2. Write Rego rules for: admission, dispatch, budget, approval, recovery, completion
3. Narrowest-scope-first evaluation; default is allow
4. Full Rego traces available for `ExplainDecision`

#### Telemetry & Cost
1. Navigate to **Telemetry + Costs** (sidebar) for: traces, metrics, logs dashboard, and the cost explorer
2. Embedded Grafana UI available at `/grafana` (Tempo / Loki / VictoriaMetrics)
3. **Overview** (default tab): total tokens / total cost / executions, plus a per-model spend panel. The Overview totals are **all-time** by default (no window is applied when none is requested), matching the Cost Explorer's per-model sum — the two surfaces always agree.
4. Cost Explorer: per-provider/model spend with drill-down (Project → Task → Execution → Model)
5. **By Workflow** tab: cost broken down per workflow run with per-step detail. Each run row's primary label is the bound work item's name (when the run is bound to a ticket), with the raw run ID demoted to a muted secondary — one-shot runs with no bound work item fall back to the truncated run ID.
6. Credits tab showing tenant-level usage

#### Settings
1. Navigate to **Settings** (replaces the former Preferences page)
2. **Appearance**: light/dark mode toggle with 28 theme variants (14 light + 14 dark)
3. **Defaults → Default models**:
   - **Default worker model**: fallback when a worker version has no `model_ref` set. If both are empty, dispatch fails (no hardcoded fallback).
   - **Default Ask Orchicon model**: model used by the Ask Orchicon conversational agent. If empty, the conversation falls back to the **free model** (`opencode/deepseek-v4-flash-free`) — surfaced in the Ask Orchicon header as a fallback warning, since that model is rate-limited and a silent provider 429 looks exactly like a "stuck" turn.
 4. **Defaults → Recovery stall parameters**: per-execution stall thresholds stored in the DB and read at dispatch time. Each field has an env-var override (`ORCHICON_STALL_*`) for dev debugging. Stall semantics: a genuine hang/loop (`no_progress`, `text_loop`, `repetition`) **hard-kills** the subprocess and routes the execution to recovery; `no_file_diff` (`no_file_progress`) is **advisory** — the subprocess keeps running, the execution gets a non-terminal `stalled` health notice, and it is revived back to `healthy` when file progress resumes. The terminal `OnResult` alone decides success/failure.
 5. **Defaults → Execution budget (defaults)**: default per-execution budget ceilings (`tokens`, `cost_usd`, `wall_clock_seconds`, `tool_call_count`) applied when a worker does not set its own value for a field. **A worker's own `budget_overrides` always overrides these per-field**; empty tenant fields fall back to the built-in defaults (tokens 1,000,000, cost $10, wall clock 3600s, tool calls 100). The wall clock is the hard per-execution deadline that kills the subprocess even if the model is still producing output (the runaway-spend backstop); an explicit `wall_clock_seconds: 0` on a worker disables that worker's timeout. Env override: `ORCHICON_STALL_WALL_CLOCK_SECONDS`.
 5. **Defaults → Execution liveness reaper**: tuning for the execution-liveness reaper (the sweep that fails executions whose runtime process is gone). The liveness probe can false-negative on a transient docker/socket hiccup, so an execution is only reaped once it is **older than the grace window** (default 60s) **and** has been reported not-alive for **consecutive-failures** checks in a row (default 3). Env overrides: `ORCHICON_REAP_GRACE_SECONDS`, `ORCHICON_REAP_CONSECUTIVE_FAILURES`.
  6. **Defaults → Execution transport resilience**: the exec stream between the control plane and the runtime supervisor can break on a transient socket/docker hiccup. The execution is **not** failed on a broken stream: the client retries (**reconnect attempts**, default 3) and the supervisor keeps the child running for the **reconnect grace** (default 60s) so a re-attach can resume. Only when the retries are exhausted (or the context was explicitly cancelled) does the execution fail and fall through to recovery. Env overrides: `ORCHICON_RECONNECT_ATTEMPTS`, `ORCHICON_RECONNECT_GRACE_SECONDS`.

#### Audit trail
The actor-based audit trail records **who did what** across the whole plane. Every mutating RPC and auth action writes an `audit_events` row **in the same transaction as the mutation** (the transactional-outbox pattern — an audit row exists iff the mutation committed); read-only calls write nothing.

- **Admin → Audit** shows two trails side by side:
  - **Events** — the actor-based trail: action (e.g. `work_item.created`, `worker.published`, `auth.login`, `conversation.message_sent`), actor identity id + auth method (`oidc`/`apikey`/`local`/`signup`/`dev`/`refresh`/`system`), polymorphic target (`target_type:target_id`), the before/after JSON snapshot for updates (creates/deletes carry one side), and the OTel `trace_id` for cross-correlation with Grafana traces. A filter bar (action / actor / target type / target id / from-to time window) scopes the `ListAuditEvents` RPC; the time inputs are `datetime-local` (browser-local) converted to UTC before they reach the query, since `occurred_at` is stored/compared in UTC.
  - **Decisions** — the existing Rego policy-decision trail (`policy_decisions`, via `ExplainDecision`), unchanged.
- `AuthService.ListAuditEvents` accepts optional `action`/`actor_id`/`target_type`/`target_id` exact filters plus a `start_time` (inclusive lower) / `end_time` (exclusive upper) window on `occurred_at` — absent bounds are unbounded (the `'epoch'` sentinel convention shared with `GetUsage`). Keyset pagination on `(occurred_at, id)` composes with the time window (the cursor is unambiguous within it).
- Ask Orchicon can query the trail too (`orchicon_list_audit_events`, read-only, with the same filters plus RFC3339 `start_time`/`end_time`).
- **What is recorded:** login/logout/signup/refresh, work item / worker / workflow / project CRUD, assign/publish/deprecate/retire, API-key create/revoke/rotate, identity + role management, tenant create, policy create/publish/supersede, webhook subscription CRUD, execution pause/resume/cancel/delete, recovery trigger/cancel/approve, settings/backup changes, runtime-image create/build/delete, approval decisions, and Ask-Orchicon conversation actions (create/delete/rename/mode, message sent, turn aborted, attachment upload).
- **Secrets never enter the trail:** before/after snapshots carry only non-secret fields — API-key hashes/prefixes, plaintext keys, tokens, passwords, worker/step prompt bodies, and policy bodies are excluded by construction.
- **Documented best-effort exceptions** (no tenant tx to join; the audit row is written in its own short tx and a failure is logged, not fatal): backup create/restore/delete (filesystem + full-DB-restore ops) and execution session messages `SendExecutionMessage`/`ContinueExecutionSession` (push into the live adapter session, no control-plane DB mutation in the handler).
- **Out of scope (by design):** reconciler/scheduler internal status flips (system churn without a user actor) and the detached Ask-Orchicon reply-collector persistence (agent output attributed to the model, not the user).

#### Ask Orchicon
1. Navigate to the **Ask Orchicon** tab in the sidebar
2. Click **+ New Conversation** to start a new chat session
3. Orchicon can:
   - **Answer questions** about Orchicon, your projects, workers, work items, and workflows
   - **Create, read, update, and delete** projects, work items, workers, workflows, and other entities
   - **Create project directories** on the filesystem with optional scaffolding (`src/`, `docs/`, `tests/`)
   - **Read a project's files** — ask "what's in my project?" to list a project's `project_dir` and read files inside it (read-only, path-traversal-safe; see §MCP & the Orchicon MCP Server)
   - **Diagnose failures** — ask "Why did the last workflow fail?" to get failure analysis
   - **Check usage and costs** — ask "How much have I spent?" for cost breakdowns
   - **View and update settings** — ask "Show my settings" or "Update my default model"
4. Chat history appears in the right sidebar — switch between or resume past conversations
5. The agent always asks clarifying questions before mutating data and refuses non-Orchicon requests

**How it works (proper MCP, not text emulation):** the chat runs on a **persistent opencode session** on the always-on host serve (the same session transport worker executions use). The first message in a conversation creates a directory-less session (`session_id` persisted on the conversation row); follow-up messages `prompt_async` the same session — no per-message `opencode run` subprocess, so follow-ups start instantly and the model recalls earlier turns from the session's own history. The built-in **Orchicon MCP server is registered by default** in the session config. `orchicon mcp` exposes the full Ask Orchicon tool registry over the Model Context Protocol (stdio JSON-RPC), tenant-scoped via `ORCHICON_MCP_TENANT_ID`. The model calls Orchicon tools natively as `orchicon_<tool>` (e.g. `orchicon_list_projects`, `orchicon_create_work_item`) through opencode's MCP integration — no string-protocol tool-call emulation. Each chat also receives an **enabled projects** context block (fresh per message) and an **About Orchicon** primer describing how the platform works. See §MCP & the Orchicon MCP Server.

**Turn lifecycle (Task 2 — send-and-ack, detached reply collection, Stop):** `ChatStream` is a send-and-ack RPC — it persists the user message, registers the in-flight turn, hands the message to the serve, and returns **immediately** with a `TurnStarted` ack carrying the assistant message id. The reply is **not** streamed to the browser: a detached collector (running on a request-independent context — a tab close / browser disconnect never cancels it) subscribes to the serve bus, drains the reply, and persists the assistant message under the acked id; the frontend **polls `ListMessages`** until it appears (the reply window is `ORCHICON_ASK_REPLY_WINDOW`, default 30m — a long multi-tool answer can never block the browser connection or be lost). **Stop** is a first-class RPC (`AbortConversationTurn`): it cancels the collector and aborts the turn on the conversation's session via the SessionClient (`POST /session/:id/abort` — no subprocess to kill); the collector persists a "Turn stopped by the user." error message and the **session stays alive** for the next message. **Serve loss mid-reply** is recovered: the collector re-attaches to the original serve/session after a bounded backoff (the host-serve watchdog preserves the data dir, so the session id survives); if the serve no longer knows the session (404 — data dir wiped), it creates a **fresh session seeded from the DB transcript** (`seedSystem` — the last 10 DB messages) and re-dispatches once. A serve that is **down at send time** (never accepted a connection for this turn) fails fast after `ORCHICON_ASK_SERVE_DOWN_GRACE` (default 15s) with a clean "serve is unavailable" error instead of looping silently up to the reply window; a serve that was already live and then drops keeps the full reply window (a restart preserves the session). Failed/timeout/stopped turns are **persisted as error messages** (empty content, `MessageMetadata.error` set) that render as an error bubble with a **Retry** affordance in the same conversation. One turn per conversation at a time: a second send while a reply is pending is rejected with `FailedPrecondition` (the frontend also disables the input). `ORCHICON_ASK_TIMEOUT` (default 60s) bounds how long an attempt waits for the serve to **accept** the sent message once subscribed (a wedged serve fails fast instead of silently queuing; subscribe reachability is bounded separately by the serve-down grace, and the reply itself by the reply window). The legacy `TextChunk`/`ToolCallResult`/`DoneSignal` stream events are retained in the proto for a future SSE surface but are not emitted.

**Stall detection (no more infinite spinners):** chat turns run a per-turn **stall monitor** (mirroring the worker-execution `progressMonitor`, minus `text_loop`/`no_file_diff` — in brainstorm mode text output IS the work, so any activity resets the clock). `no_progress` trips when no `text`/`reasoning`/`step_finish`/`tool_use` arrives for our session within `ORCHICON_ASK_STALL_NO_PROGRESS_WINDOW` (default **120s**); `repetition` trips when the same tool-call signature repeats more than `ORCHICON_ASK_STALL_REPETITION_COUNT` (default 5) times within `ORCHICON_ASK_STALL_REPETITION_WINDOW` (default **300s**). On a trip the collector **aborts** the conversation's opencode session (the same abort Stop uses — the model stops generating NOW) and persists a clear, retryable "The model appears to be stuck (…). The turn was interrupted — send a message or retry to continue." error under the acked id — instead of the user watching a spinner until the 30-minute reply window.

**Interject (the chat nudge with an interrupt):** `InterjectConversationTurn` is a server-streaming RPC (same response shape as `ChatStream`) that lets the user **steer an in-flight turn** — the chat equivalent of a worker-execution nudge, but it **supersedes** rather than queues: the running turn's collector is cancelled, the conversation's opencode session is **aborted** so the model stops generating NOW, and the interjection is dispatched on a fresh turn that acks a **new** assistant message id and streams live. The superseded turn's partial content is persisted as a **plain assistant message** (skipped entirely when nothing arrived — no error bubble; the interjection is intentional, not a failure). Idempotent: interjecting with nothing running behaves exactly like `ChatStream`. The frontend enables the input **while streaming**: sending mid-reply routes to the interject RPC, and Stop remains available alongside Send — a conversation can never be stuck behind a wedged turn.

**No orphaned turns:** the in-memory turn registry is token-guarded and TTL'd. Every entry carries a token; `remove` only deletes when the token matches, so a superseded collector's finalize can never clobber the replacement turn (the stale-finalize race is eliminated by construction). A background sweeper evicts entries older than `ORCHICON_ASK_TURN_MAX_AGE` (default: reply window + slack, 31m), cancelling the collector and **aborting the serve session** — the "every turn has a hard backstop" guarantee: a collector that can never finalize is reaped in bounded time instead of blocking the conversation until a server restart. In the UI, a dropped ChatStream socket on an **acked** turn keeps the stream slot attached (the Stop button + interject input stay; a "Connection lost — still working…" notice appears) and completion is resolved by the existing `ListMessages` poll, which works across socket drops, page reloads, and server restarts because the detached collector persists the reply regardless.

**Reasoning stream + persistence (Task 3):** reasoning (thinking) parts are unwrapped from the SSE bus in the live turn loop via the same `LegacyEventFromBus` mapping executions use (`{"type":"reasoning","part":{...}}` — only at part end, `time.end` set), accumulated separately from the reply text (never folded into assistant content), and **persisted as reasoning chunks on the assistant message** (`ask_orchicon_messages.reasoning jsonb NOT NULL DEFAULT '[]'`, a JSON array of strings — one entry per reasoning part, boundaries preserved). Partial reasoning is preserved on error/stop/timeout turns too. `ChatMessage` exposes `repeated string reasoning = 10` (delivered via the existing `ListMessages` poll — the frontend renders thinking bubbles from it) and the `ChatStreamResponse` oneof gains a typed `ReasoningChunk` event (field 7, defined-but-not-emitted like `text_chunk` — retained for a future SSE surface). The data path is additive: a model that emits no reasoning parts yields no events and an empty array.

**Mode model + per-mode prompts (Task 4):** a conversation has a **mode** (`ask_orchicon_conversations.mode`, default **brainstorm**) selecting the persona applied per message. `Conversation.mode` is a proto enum (`CONVERSATION_MODE_BRAINSTORM` / `CONVERSATION_MODE_ORCHICON`; the DB stays a text column, `brainstorm`/`orchicon`, validated at the API boundary); new conversations default to **brainstorm** ("What can I help you create today?" — a deep systems-thinking partner where general design, coding, and brainstorming are in scope). **Orchicon** mode is the strictly-governed platform expert (byte-identical to the pre-Task-4 single persona: refuses non-Orchicon/general-coding/personal requests). **Toggling is session-free:** the mode is read from the conversation at turn-dispatch time and applied as opencode's **per-turn `system` field** (`prompt_async`), so the SAME opencode session persists across a switch — no session change, no serve restart, no history re-seed; the next message simply carries the new persona. The `SetConversationMode` RPC (`SetConversationModeRequest{id, mode}`) is the toggle surface (wired to the F4 task 9 UI), and `CreateConversationRequest` accepts an optional `mode`; unknown enum values are rejected with `CodeInvalidArgument`. **MCP surface is identical in both modes** — MCP servers are serve/session-scoped and cannot be swapped per turn, so both share the full Orchicon MCP (`orchicon_*` tools); the difference is the system prompt (governance), not the tool set. **Config semantics:** the DB `role/skills/behavior/agents_md` customization applies to **Orchicon mode only** (its defaults carry the "refuse general coding help" governance that would contradict the open mode); the DB `system_prompt` ("Additional Instructions") is appended in **both** modes — the shared tenant-customization surface. Model resolution is unchanged (mode does not select a model). The seed/reuse split (DB history injected on a fresh session vs. already in the live session) is orthogonal to mode and unchanged.

#### MCP & the Orchicon MCP Server

The binary ships an **MCP server** subcommand (`orchicon mcp`) that exposes Orchicon's tool registry over the Model Context Protocol (JSON-RPC 2.0, newline-delimited stdio). It is consumed by opencode and any other MCP client (Claude Desktop, Cursor, …).

- **Discovery/execution**: `tools/list` returns every registered tool with a JSON-schema input; `tools/call` executes it (failures come back as `isError` results, per the MCP spec). The server echoes the client's MCP protocol version so the handshake succeeds with any client (opencode 1.18 sends `2025-11-25`).
- **Tenancy**: the server is scoped to one tenant per process via the `ORCHICON_MCP_TENANT_ID` env var, set by the control plane through the opencode config `environment` map of the injected MCP entry. Unset (e.g. a human wires `orchicon mcp` into Claude Desktop manually) → dev tenant with a warning.
- **Default registration**: `BuildConfigContent` (the `OPENCODE_CONFIG_CONTENT` injected into every opencode run) now registers the built-in Orchicon MCP by default — for **in-process worker executions** and **Ask Orchicon chat** (both co-located with the plane's Postgres). It is deliberately **not** registered for **runtime-container executions**: the per-workflow runtime container is an isolated, root-free sandbox with no network route to the plane's Postgres, and handing it the DB DSN would break the security model. The user's own opencode-config MCP servers are still merged in everywhere.

##### Project-directory context tools

Ask Orchicon runs on a **directory-less** session (the no-project execution guard blocks general-purpose file tools), so it needs a dedicated surface to see a project's files. Two **read-only** `orchicon_*` tools close that gap for **both** conversation modes (registered in `allTools()`, so they appear in every mode's prompt and in the MCP server with zero per-mode plumbing):

- **`orchicon_list_project_dir`** — shallow one-level listing of a project's `project_dir` (or a subdirectory of it). Params: `project_id` (required), `path` (optional; a relative subpath or an absolute path inside the root — defaults to the root). Skips VCS/build noise dirs (`.git`, `node_modules`, `vendor`, `dist`, `build`, `.venv`, `__pycache__`, `.orchicon`, `.cache`), caps at 1000 entries (`truncated: true` beyond), and reports each entry's `type` (`dir`/`file`/`symlink`) from `Lstat` — never following or descending into symlinked directories.
- **`orchicon_read_project_file`** — reads a single file inside `project_dir`. Params: `project_id` (required), `path` (required), `max_bytes` (optional, default 256 KiB, clamped to `[1, 256 KiB]`). Returns a JSON envelope (`{project_id, path, bytes, truncated, content}`) so real content is unambiguous from a truncation marker; a directory target errors with a "use list_project_dir" hint.

**Path-traversal safety** is the contract, enforced by one shared resolver — `contextfiles.ResolveWithin` (`internal/contextfiles`), the same leaf package that validates project/work-item context files. Defense in depth: trim + length-bound the caller's path; normalize the root through `filepath.Abs` + `EvalSymlinks` (a `project_dir` may live under a symlinked home); build the target lexically (absolute cleaned as-is, relative joined to the root); reject any target whose `filepath.Rel` to the root is `..` or escapes upward (`..`, `..\`-style, absolute out-of-root); then fully evaluate the target with `EvalSymlinks` and re-check containment against the evaluated root — a symlink inside the root that resolves **outside** is rejected, one that stays inside is allowed (legit generated links), and a nonexistent/broken target keeps its lexical form so the downstream op reports the not-found. The traversal tests (`TestResolveWithin*` in `internal/contextfiles`) are pure-filesystem unit tests that **always run** in CI (no DB), covering `..` escapes, absolute out-of-root, symlinked-file escapes, symlinked-directory escapes, nested symlinks, allowed in-root symlinks, symlinked roots, empty/overlong paths, and nonexistent targets; the DB-backed `tool_project_dir_test.go` proves the tools wire the resolver in end-to-end. Residual, documented risk: a TOCTOU window between the `EvalSymlinks` check and the `ReadDir`/`Open` (the same-user agent on an operator-owned filesystem — a prompt-injected model, not a racing local actor — is the threat model; open-by-handle hardening was considered and deferred).

The tools work for projects found via the existing project tools **or one just created in-conversation** (`create_project`/`create_project_directory` + `update_project` all set `project_dir`); a project with no `project_dir` errors with guidance. This is the **only** route to project files — sessions stay directory-less by design.

##### Work item field coverage in the MCP

Every field the Connect API lets a client set on a work item is settable through the MCP tools. The tools reuse the Connect service's shared validators (`internal/workitem`) and mirror its downstream effects (status→scheduled flip, auto-start trigger, outbox events), so the two surfaces cannot drift:

| Field | `create_work_item` | `update_work_item` | Notes |
|---|---|---|---|
| `project_id` | ✓ (required) | ✓ | update = reassign; target must be active |
| `title` | ✓ (required) | ✓ | trimmed, max 500 chars |
| `kind` | ✓ | ✓ | epic/feature/task/subtask; kind switch resolves the hierarchy |
| `parent_id` | ✓ | ✓ | hierarchy rules enforced (only epics are top-level) |
| `description` | ✓ | ✓ | markdown |
| `acceptance_criteria` | ✓ | ✓ | markdown |
| `acceptance_review` | ✗ (auto-populated) | ✓ | markdown; generated by the WorkflowReconciler when a bound run completes |
| `priority` | ✓ | ✓ | 1–5 |
| `budgets` | ✓ | ✓ | JSON object, validated |
| `context_window` | ✓ | ✓ | int |
| `workflow_id` | ✓ | ✓ | empty on update clears the binding |
| `scheduled_start_at` | ✓ | ✓ | ISO 8601 or "N minutes from now"; setting it flips status → `scheduled` (ADR-001) |
| `auto_start_workflow` | ✓ | ✓ | opt-in (default false); true + no schedule clears any existing schedule and starts the bound run immediately |
| `runtime_image` | ✓ | ✓ | empty = base image |
| `context_files` | ✓ | ✓ | absolute file OR directory paths (same model as projects); empty list on update clears |
| `workflow_run_id` | ✗ | ✓ | update-only (create starts empty) |
| `status` | ✗ (derived) | ✓ | `pending, scheduled, ready, assigned, running, checkpointing, succeeded, failed, cancelled, recovering, recurring` |
| `assigned_worker_ref` | via `assign_worker`/`unassign_worker` tools | same | mirrors the `AssignWorker`/`UnassignWorker` RPCs (`worker_id` + `version`) |
| `sort_order` | via `reorder_work_items` tool | same | reorders a sibling group (sequence chain order) in ONE transaction; mirrors the `ReorderWorkItems` RPC — display sort never mutates it |

**Read-only (never settable via the API or the MCP):** `id`, `tenant_id`, `version`, `created_at`, `updated_at` (server-managed), `workflow_step_id`, `results`, `prompt_context` (reconciler-written JSONB).

MCP work item mutations honor the transactional outbox pattern (invariant #3): `work_item.created` / `work_item.updated` / `work_item.kind_changed` / `work_item.worker_assigned` / `work_item.worker_unassigned` rows are enqueued in the same transaction as the write, exactly like the Connect handlers. After commit, `auto_start_workflow=true` calls `workflow.StartWorkflowDirect` — the same real service call the API path makes — so "start immediately on save" behaves identically whether triggered from the UI, the API, or the MCP.

### Authentication

- **Embedded OpenID Provider** (`internal/auth/op`): the plane serves a real
  OIDC authorization-code + PKCE flow on its own origin via zitadel/oidc v3
  (`pkg/op`). Endpoints: `/.well-known/openid-configuration`, `/authorize`
  (+ `/authorize/callback`), `/token`, `/userinfo`, `/jwks`. ID tokens are
  signed **ES256** with a keypair deterministically derived from
  `ORCHICON_AUTH_SIGNING_KEY` (HKDF-SHA256 → P-256 scalar base mult) so the
  JWKS publishes only the public point — never the HMAC access-token secret.
  The login bridge (`/auth/op/login`) authenticates the caller's existing
  Orchicon session cookie and marks the authorize request done; without a
  session it bounces to the SPA `/login?next=...` (same-origin paths only).
  Env: `ORCHICON_OP_ENABLED` (default true), `ORCHICON_OP_REDIRECT_URIS`,
  `ORCHICON_OP_ISSUER` (empty = dynamic from the request Host). The existing
  BYO-IdP RP (`internal/auth/oidc.go`) is unchanged: PKCE is capability-gated
  on the IdP advertising `S256` in discovery, so non-PKCE IdPs get the same
  byte-for-byte flow as before.
- **Local accounts (embedded IdP passwords)** — `local_credentials` table
  (tenant-scoped, `tenant_isolation` RLS), one row per (tenant, identity),
  keyed to `identities.id` (FK `ON DELETE CASCADE`). Only the **password
  hash** is stored — argon2id (RFC 9106 m=64 MiB/t=3/p=4) PHC strings by
  default, bcrypt (`$2a$`/`$2b$`/`$2y$`) accepted on verify via prefix
  dispatch; plaintext is never persisted, logged, or returned. This is a
  deliberate, narrow amendment to the AGENTS.md "passwords are never stored
  by the control plane" standard: human passwords live **only inside the
  identity-provider boundary** (`internal/auth` + `internal/auth/op`), never
  in control-plane business logic — no service, RPC, or Ask Orchicon tool
  outside the auth boundary touches a credential row. Login is `POST
  /auth/local-login` (`{username, password, next?}`, gated on
  `ORCHICON_OP_ENABLED`): it verifies the hash, mints the Orchicon token pair
  (HttpOnly refresh cookie), and — when `next` is the OP login-bridge path —
  completes the pending authorize request so the local account finishes a
  full OIDC flow without a prior session. Failures return a generic 401 (no
  user-enumeration hint); no identity is auto-provisioned by a login.
  **Self-service sign-up** is `POST /auth/signup` (`{username, password,
  next?}`, gated on the same `ORCHICON_OP_ENABLED` — sign-up availability
  *is* the embedded IdP being on): it atomically creates a fresh identity +
  argon2id-hashed local credential in the deployment tenant (duplicate
  username or an already-existing identity → generic 409, which also blocks
  identity squatting on BYO-IdP handles), then runs the local-login tail
  verbatim — token pair + HttpOnly refresh cookie + OP authorize completion.
  A signed-up account is a plain `user` identity with **zero entitlements**
  (no admin grant on an open endpoint); the bootstrap admin stays the sole
  initial admin and operators grant roles later via Admin → Identities.
  The SPA `/signup` route (linked from `/login` only when the plane
  advertises it) collects username + password and lands the user in the app
  in one step.
  Provisioning is the admin-only `AuthService.SetLocalCredential` RPC
  (`auth:write`), which hashes at the boundary and never returns the hash.
  The SPA `/login` route shows the username+password form.
- **Local dev**: The embedded OpenID Provider is the default IdP — no
  external IdP needed. OIDC is the base auth path in **every** mode: there
  is no anonymous dev bypass (a request without a credential is 401
  everywhere) and the synthetic `/auth/dev-login` is a flag-gated escape
  hatch (`ORCHICON_DEV_LOGIN`, default **off**, returns 403 when disabled).
  A fresh local plane seeds a first admin for the embedded-OP login
  (local mode + embedded OP only, `ORCHICON_LOCAL_ADMIN_SEED=0` to opt
  out; username/password pinned via `ORCHICON_LOCAL_ADMIN_USERNAME` /
  `ORCHICON_LOCAL_ADMIN_PASSWORD`, random + logged once at boot when
  unpinned) so the UI is usable out of the box. The seed is idempotent
  and never clobbers an existing admin; to re-arm it as lockout recovery
  (lost the admin password), set `ORCHICON_LOCAL_ADMIN_RESET=1` — same
  guards (local mode + embedded OP only, explicit opt-in, never a
  default), it overwrites the admin credential on next boot while keeping
  the identity and admin role binding, and it works even on a plane with
  `ORCHICON_LOCAL_ADMIN_SEED=0`. Config validation requires
  an issuer (embedded OP **or** external IdP) in every mode. The public
  `GET /auth/config` endpoint mirrors the plane's auth capabilities
  (`embedded_op` / `external_oidc` / `dev_login` / `signup`) for the honest
  login page; the unauthenticated SPA redirects to `/login`.
- **Changing a local-account password** — Admin → Identities → "Set
  local password" (calls the admin-only `SetLocalCredential` RPC,
  `auth:write`; the plaintext is argon2id-hashed at the boundary and the
  hash is never returned). This is the documented way to change the
  default admin's credentials after first boot.
- **Identity lifecycle (create / edit / disable / delete)** — Admin →
  Identities (the same tab, not a new surface) drives four admin-only
  `AuthService` RPCs, all gated to `auth:write` by the RBAC interceptor:
  - `CreateIdentity` (`{identity_type, subject, display_name}`) provisions a
    `user` or `service` identity in the caller's tenant. For `user` the
    subject is the login handle and must match the local-account username
    charset `^[a-z0-9][a-z0-9._@+-]*$` so the identity can immediately get a
    `SetLocalCredential` whose username matches its subject; for `service`
    the subject is optional (slug charset, ≤63) and a synthetic `sa-<ULID>`
    is generated when omitted — the natural flow for a machine account is
    create identity → create an API key bound to it. Duplicate
    `(tenant_id, subject)` → 409 AlreadyExists.
  - `UpdateIdentity` (`{id, display_name, version?}`) renames an identity
    with optional optimistic concurrency (`version` mismatch → 404).
  - `SetIdentityStatus` (`{id, status}`) flips between `active` and
    `disabled` (the only writable values; anything else → 400). Disabling
    is reversible (enable) and is the UI's Disable/Enable toggle. Note: a
    disabled identity's **API keys are not yet blocked at resolution time** —
    key-level enforcement is a documented follow-up; disable revokes
    nothing credential-wise on its own.
  - `DeleteIdentity` (`{id}`) hard-deletes the identity plus its role
    bindings, API keys, and local credentials in one tenant-scoped
    transaction. Two server-side guards protect the admin surface: the
    caller cannot delete the identity they are authenticated as (400), and
    an `active` identity must be disabled first (412 FailedPrecondition) —
    the UI only ever deletes disabled rows.
  There is no schema migration (the `status` column already exists, default
  `active`) and no outbox event emission (consistent with the sibling
  AuthService mutations); identity lifecycle webhook events are a follow-up.
  The SPA list follows the standard list-page pattern (search, status
  filter, select-all + per-row checkboxes, selection count, bulk
  Disable/Delete on ≥1 selected).
- **Production**: Real OIDC issuer with authorization-code flow (BYO IdP);
  the embedded OP can be disabled with `ORCHICON_OP_ENABLED=false`
- **API keys**: SHA-256 hashed, least-privilege scopes for headless/CI clients
- **Frontend**: Access tokens in memory; refresh tokens in HttpOnly cookies.
  A router-level auth guard (`beforeLoad` on the root route,
  `frontend/src/auth/route-guard.ts`) protects every app route at
  navigation time — unauthenticated visitors are redirected to `/login`
  before any protected component renders, with the intended destination
  preserved in `?next=` so a successful login returns there (SPA-side
  navigate; only server-only OP-bridge paths full-page-load). `/login`,
  `/signup` and `/auth/callback` are the explicit public-route allowlist. On a full page
  load the guard and `AuthProvider` share one `ensureSession()` bootstrap
  that exchanges the HttpOnly refresh cookie for a new access token, so a
  live session survives reloads without re-login; the AppShell effect
  remains as the reactive mid-session safety net. Sign-out clears the
  in-memory token, fires `POST /auth/logout` to expire the HttpOnly refresh
  cookie server-side, and arms a signed-out flag (`session.ts`) that stops
  the guard from silently re-authenticating via a still-valid cookie — a
  signed-out user stays signed out across reloads and SPA navigations
  (AC1). `POST /auth/logout` is credential-free by design, so it is on the
  `ResolveAuth` public-path allowlist (`internal/middleware/auth.go`) —
  the auth middleware would otherwise 401 the logout before the refresh
  cookie could be cleared, letting a reload after sign-out re-authenticate.

### Tenancy model

**Decision (org-groups / deployment-scoped): each Orchicon deployment owns exactly one tenant.** The deployment is the isolation boundary; all identities, projects, work items, executions, and audit records are tenant-scoped to the seeded deployment tenant through the RLS machinery. The deployment tenant is **config-driven**: `ORCHICON_DEPLOYMENT_TENANT_ID` (default `tnt_dev`) is the single source of truth, validated at boot (non-empty, lowercase alphanumerics plus `-`/`_`, ≤63 chars — a misconfigured value fails boot, never seeds a second tenant).

Every auth path resolves logins into the deployment tenant — the OIDC callback, dev-login, the embedded-OP local login, and the local-admin bootstrap all read `Config.DeploymentTenantID`, never a code literal. No subject→tenant routing rules: the IdP's identity claims (`org`, `groups`, `tenant`, …) are **not** consulted for tenant selection; claim-based routing and first-login assignment are deferred to the SaaS forward path.

Consequences for the three features gated on this decision:

1. **Sign-up tenant targeting** — self-serve sign-up over the embedded IdP (`POST /auth/signup`, `internal/auth/handlers.go`) creates identities + local credentials in the **deployment tenant** (no role grant); identities are otherwise operator-provisioned (embedded-OP local accounts via `SetLocalCredential`, or BYO IdP). **Tenant provisioning is idempotent boot-time seeding**: `db.SeedDevTenant` inserts the configured deployment tenant id (`tenants` row, slug mirrors the id so the unique slug index can never collide) before auth mounts, so a fresh install with `ORCHICON_DEPLOYMENT_TENANT_ID=acme` boots into `acme`. On the SaaS forward path this becomes self-serve tenant provisioning + admin `CreateTenant`.
2. **Audit-trail scoping** — by construction: the `audit_events` table (RLS-enabled, forward-only migration `db/migrations/20260815000000_audit_events.sql`) carries `tenant_id` + the standard `tenant_isolation` RLS policy, so a deployment's audit trail is its own and cross-tenant audit reads are impossible via RLS.
3. **Identity provisioning & membership** — per-tenant `EnsureIdentityForSubject` upsert is kept; the OIDC callback resolves the tenant from deployment config, not a code literal. **Membership is the role-binding model**: an identity gains membership in the deployment tenant on first login by being upserted there and by holding the tenant `admin` role (first-login grant, idempotent); within-tenant access control stays with tenant/project RBAC scopes. Cross-tenant membership does not exist in a single-tenant-per-deployment install; the schema supports it later.

The multi-tenant schema (`tenants` table, RLS, admin `CreateTenant`/`ListTenants`) is **retained unchanged** as the forward-compatible foundation for a future SaaS phase — a SaaS pivot would be additive (subject→tenant routing at the OIDC callback, self-serve tenant provisioning), not a rewrite. The tenants admin surface stays admin-only and is not productized in this phase.

The remaining hardcoded `tnt_dev` literals outside auth (MCP / recovery / scheduler / sequence / runtime / seed paths) are swept to the same config value as documented follow-ups of decision #178; the auth surface above is the first consumer of the shared config field.

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
  - **project dirs/files from a manifest**: the control plane writes `/var/lib/orchicon/project-mounts` (every `project_dir` + `context_files` from the projects table **and every work item's `context_files`**, refreshed every 30s). `container.sh up`/`rebuild` mounts each listed path at its host location. **After you save a project dir or context files in the UI, run `scripts/container.sh sync-mounts [dev|prod]`** to apply — Docker can't add bind mounts to a running container, so `sync-mounts` compares the manifest to the live container's mounts and recreates it when any are missing.
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
> **Orchicon MCP availability:** the built-in Orchicon MCP server is registered by default for **in-process** executions and Ask Orchicon chat. Inside runtime containers it is **NOT** registered on base/`:gui` images — the sandbox has no route to the plane's Postgres and is deliberately kept DB-credential-free (§MCP & the Orchicon MCP Server). On **`:orchicon-dev`** images the container's serve registers it against the **in-sandbox plane's** Postgres instead (§Sandbox plane below) — workers get `orchicon_*` tools against their own disposable DB, never the host plane's.

**Components:**

- **`orchicon runtime-daemon`** (host process): the only process with access to the Docker socket. Serves a narrow HTTP API over a unix socket (default `/tmp/orchicon-runtime/runtime.sock`, bind-mounted as a **directory** into the supervisor container at `/var/run/orchicon-runtime`): lease/release warm runtime containers (`POST`/`DELETE /v1/runtimes`), build/remove runtime images (`/v1/images`). Every request is validated — image allowlist (the base + `ORCHICON_RUNTIME_IMAGES` stock images + any locally-present image carrying the inherited `org.orchicon.runtime-base` label), mount sources restricted to the projects root — so the control plane can never create an arbitrary container. The daemon owns the **warm pool** (§below): leases are daemon-resident, the pool is reset wholesale at daemon start (which also reaps plane-down leaks), and clean containers are idle-reaped. Started by `scripts/container.sh up`; manage with `scripts/container.sh runtime-daemon` / `runtime-stop`.
- **`orchicon runtime-supervisor`** (PID 1 inside each runtime container): listens on a unix socket (`/tmp/orchicon-agent.sock`) and answers the daemon's two requests — a `ping` (container readiness) and the `serve` handshake. Hosts the container's **opencode serve** (detached child, `Cmd:"serve"`, 0.0.0.0:4096, guard + **stable** XDG data dir `/tmp/orchicon-serve-data` so sessions survive restarts), answers idempotent serve handshakes (owning the serve password, liveness-gated — a wedged serve is restarted, not reported as up), and runs a **serve watchdog** that polls `/global/health` and restarts the serve in place (same port + password) when it stops answering. On images that bake the pieces (currently `:orchicon-dev`) it also boots the **sandbox plane** (§Sandbox plane below) in the background. Builds the execution-guard shim in-container so workers run under the same `rm`/`sudo`/`dd`/`mkfs` path-scoped safety guard as the in-process path. (The one-shot exec/stream/reconnect machinery was removed with the transport it served.)
- **`orchicon runtime-client`** (in-container): forwards a request from the daemon (via `docker exec`) to the supervisor socket and relays the answer back, so the daemon never needs shell-level access to the container.

**The orchicon binary is mounted, never baked:** the runtime images contain **no `orchicon` binary** — the daemon bind-mounts **its own executable** read-only at `/usr/local/bin/orchicon` in every runtime container it creates, so the container can exec `orchicon runtime-supervisor` / `runtime-client` without the binary being baked into the image (the same "mount, never bake" pattern as the adapter CLIs). The daemon CLI **self-copies its binary to a stable path next to the socket at startup** (`cmd/orchicon/runtime.go` `copySelf`), so dev hygiene that deletes the original (`make clean` removes `bin/orchicon`) can never orphan the mount — the copy is what gets mounted, refreshed only when the daemon is rebuilt and restarted. The mount is a **hard dependency**: the entrypoint is `orchicon runtime-supervisor`, so if the executable is unavailable the daemon **fails the container create with a clear error** rather than creating a container that would exec a missing binary and die (never a silent skip).

**Lifecycle (warm pool + serve gate):** the `WorkflowReconciler` **leases** a warm runtime container when a run leaves `pending` (mounting the project's `project_dir` plus any project/work-item `context_files` paths that lie outside it) and **releases** it when the run reaches terminal — the release resets the container in the background (§Warm Pool below). **The run-start serve gate** (`runtime_ready`, §Persistent Worker Sessions) guarantees no execution is dispatched until the container's opencode serve is **proven usable** (L1: `/global/health` + a real session-create round-trip). A 30s adopt pass at boot ensures a lease exists for every active run (the daemon's pool reset at start covers orphaned containers from a plane-down/daemon-restart). **Instance-scoped**: every runtime container is labeled with its owning instance (`orchicon.instance=dev\|prod`), so dev and prod sharing one daemon never reap each other's runtimes. The same boot pass runs an **execution-liveness reaper**: executions still `running` whose session runner is gone (plane restart, lost runtime container) are failed with `execution lost: control plane restarted or runtime container gone` and their work item transitions to failed, so the workflow's recovery step re-dispatches in a fresh runtime instead of the run getting stuck. The adapter **self-heals** on dispatch too: it re-leases the run's container before every execution (the daemon's checkout is idempotent per run), so a recovery re-dispatch can't race ahead of the adopt pass. Headless `orchicon serve` (no daemon socket) disables runtime containers, stays in-process, and still reaps in-process executions orphaned by a restart.

**Warm pool** (`internal/runtime/pool.go`): runtime containers are keyed by **environment** (image + project mounts) and reused across runs of the same project instead of cold-started per run — the container creation + serve bring-up that used to happen on the dispatch hot path now happens once, at lease time, off the hot path. Leases are **exclusive per run** (a container is handed to exactly one run at a time). On release, the container is **reset in the background** — `docker rm -f` + recreate with the identical spec + warm the serve — so the pool only ever hands out **pristine** environments: nothing from the previous run's state (installed packages, `/tmp`, opencode sessions/data) crosses the boundary, preserving the security property that motivated the sandbox. Dispatch never blocks on a reset — a checkout that finds the pool empty just creates fresh (the cold path, which the run-start gate absorbs). Clean containers are idle-reaped (`ORCHICON_RUNTIME_POOL_IDLE`, default 10m) and capped per environment (`ORCHICON_RUNTIME_POOL_CAP`, default 1). Two concurrent runs on the same environment each get their own container (on-demand create) — never shared.

**Stuck-run detection (no leaked containers):** a container is only reaped when its run reaches a terminal state, so any run that can never progress would hold one forever (the adopt sweep treats every running run as active and keeps the container alive). The reconciler therefore fails a run **at start** — reaping the container and, for a sequence child, halting the parent's chain — when the published version has an **empty step DAG** (`steps=[]`), when its workflow **version row is gone** (workflow deleted / raw-seeded run), or when the runtime image can't be resolved (this failure is now committed, not rolled back by the deferred rollback). It also un-wedges **orphaned step references**: `pollTaskStep` fails a running task step terminal when its work item was hard-deleted mid-run, and falls through to the recovery block (after the dispatch-link grace) when its execution row is gone — instead of waiting forever, which previously left the run `running` and its container up indefinitely.

**Runtime adapter CLIs are mounted, never baked:** the images contain **no adapter binary**. The daemon mounts the operator's host `~/.opencode` install (read-only) into every runtime container and puts its `bin/` on PATH, so the supervisor can exec `opencode` — the same mount `container.sh`/`orchicon install` add to the main container for in-process dispatch. The supervisor's `argv[0]` allowlist (`runtimeBinAllowlist` in `internal/runtime/agent.go`) lists the adapter binaries Orchicon may exec — `opencode` today; `claude`/`codex` get one added entry when those adapters land. This is the licensing-safe pattern for all future adapters: **the product mounts the operator's own install; it never ships, downloads, or redistributes the CLI.**

**Security model — no root process in the runtime container:**

- The runtime container runs as the **host user's uid** (`ORCHICON_HOST_UID`, default 1000) with the image rootfs **chowned to that uid**, so workers have full write control over the ephemeral filesystem (they can install tools) while any bind-mounted project directory is written as the host user — never as root. A worker cannot `chown` a project file to root or escalate to the host.
- `dpkg` refuses to run as non-root, so system packages (python, node, build-essential, gh, …) are **baked at build time** (`deploy/runtime/Dockerfile`); runtime installs use user-space package managers (`pip` with `PIP_BREAK_SYSTEM_PACKAGES`, `npm`, `mise`, `uv`, `curl`) into the chowned rootfs / ephemeral `$HOME`.
- The daemon mounts `~/.config/opencode` and `~/.local/share/opencode` **read-only**; the supervisor redirects each worker's opencode state to an ephemeral `XDG_DATA_HOME` under `/tmp` (seeded with `auth.json`), so sessions/keys never touch the host's real opencode data. Git identity + credential store are mounted read-only (PR/merge workers need them).
- Per-runtime resource limits: 4 CPU / 4 GB memory / 2 GB tmpfs `/tmp` (configurable via `ORCHICON_RUNTIME_CPUS` / `_MEMORY` / `_TMPFS` on the daemon).

**Runtime images (self-service builds):** the image a workflow run's container uses is chosen **per work item** (`work_items.runtime_image`, backend-stamped to the base image when empty). The **Runtime Images** page (sidebar) lets you define and **build** custom images on the host runtime daemon: a structured form (apt packages, toolchain lines, env) with a **live Dockerfile preview** that doubles as an advanced raw-Dockerfile editor, plus a **Deploy** button that streams the `docker build` log. Editing a ready image reverts it to draft so it must be rebuilt; delete removes the spec row (custom images also remove the local Docker image — gated on no active run using it; canned rows and the daemon's base image are never `docker rmi`'d). Every build is guaranteed to derive from the base image — the daemon rewrites the Dockerfile's `FROM` line to the base and injects the `org.orchicon.runtime-base=true` label (plus `org.orchicon.runtime.spec-version=<n>`, the spec version the image was built from), which is also the container-create gate (a locally-present image carrying that inherited label is accepted without a separate registration). **Deploy is idempotent**: the image's `version` is the "spec changed" signal (spec edits bump it; build-flow status transitions do not), and the row records `built_version` — the version the current `ready` image was actually built from. Re-deploying an unchanged ready spec short-circuits (`built_version == version` → instant "up to date", no `docker build`, no prune); editing the spec bumps `version`, so the next Deploy rebuilds. The workflow-run start resolves the image (template → bound work item; one-shot → the WORK_ITEM markers' items, all must agree or the run fails at start) and stores it on the run, so a self-healed container is recreated with the identical image.

**Canned (stock) runtime images:** the shipped images — the **base**, **`:gui`** (`deploy/runtime/Dockerfile.gui`, headless Qt/tkinter/X11 libs) and **`:dev`** (`deploy/runtime/Dockerfile.dev`, the dogfooding image — Go/Node/buf/atlas plus a baked PostgreSQL 15 for in-sandbox DB testing) — are **seeded as normal, editable `runtime_images` rows (`source='stock'`) on every boot**, exactly like canned workers. They appear in the main Runtime Images list (with a **stock** badge + spec version/built_version), are editable in the advanced Dockerfile editor, deployable, and deletable (a deleted canned row is re-seeded next boot). The seeder writes the shipped Dockerfile template plus a versioned seed marker (`# orchicon.seed=<sha12>`); a row that is still an intact seed is reconciled in place, a stale seed (template changed) is rolled forward — `version` bumps, `built_version` lags, and the row is **auto-built** asynchronously, which prunes the previous version of the tag — while any row whose body no longer matches its marker is a user edit and is **never touched**. When the local docker image is already current (container.sh / the installer built it before the plane booted) the seeder records `built_version = version` instead of rebuilding. The daemon's `GET /v1/images` now reports each image's `org.orchicon.runtime.version` / `spec-version` labels so the seeder can reconcile. Instances are independent: dev and prod each seed their own canned rows in their own DB.

**Dogfooding Orchicon (the `:dev` image):** to have a worker build and test the Orchicon repo itself, set the project's `project_dir` to your Orchicon checkout on the host and give its work items the `:dev` runtime image. The per-workflow runtime container mounts `project_dir` automatically at container-create time (`Lifecycle.EnsureForRun` reads it from the projects table — no `sync-mounts` needed for runtime containers; that script only applies to the long-lived single-container instance, where Docker can't add bind mounts to a running container). The checkout must be under the daemon's `AllowedRoots` (default `$HOME/projects`). Inside the sandbox the supervisor has **already booted the full Orchicon control plane** (§Sandbox plane below) — Postgres, NATS and `orchicon serve` — so a worker can `curl http://localhost:8080/healthz`, run `go build`/`go vet`/`make gen`/`make fe-build`, the full `make ci` DB path against `localhost:5432`, and use the `orchicon_*` MCP tools against the sandbox DB, with no ad-hoc booting. The whole plane dies with the container and never touches the plane's Postgres, preserving the no-DB-route sandbox invariant.

**Sandbox plane (full Orchicon environment in-container):** the `:orchicon-dev` runtime image bakes PostgreSQL 15 + `nats-server` (v2.10, matching the single-container image's line), and the runtime supervisor boots a **disposable, self-contained Orchicon control plane** at container start — `initdb` (once, into the stable data dir `/var/lib/orchicon-sandbox/postgres`) → `pg_ctl -w start` (trust auth, `listen_addresses=localhost`, socket dir under the sandbox data dir) → `createdb orchicon` → `nats-server -js` → `orchicon serve` with sandbox env (`ORCHICON_POSTGRES_DSN=postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable`, `ORCHICON_NATS_URL=nats://localhost:4222`, `ORCHICON_HTTP_ADDR=:8080`, `ORCHICON_GRPC_ADDR=:9090`, `ORCHICON_TELEMETRY=none` — no Grafana stack, `ORCHICON_OPCODE_SESSION_TRANSPORT=0` — the plane is an API/DB/MCP surface, never a second execution plane, `ORCHICON_SANDBOX_PLANE=1` — suppress the serve's kill-orphans pgrep so the supervisor's live serve is never SIGTERMed, `ORCHICON_INSTANCE=sandbox`, `ORCHICON_BLOB_DIR` + `ORCHICON_INDEX_CHECK_INTERVAL=0` under the sandbox data dir). The plane runs migrations + tenant/worker seeding on boot against the sandbox DB.

  **Self-gating:** the supervisor probes for a Postgres server bin dir (`/usr/lib/postgresql/<ver>/bin` containing `initdb`/`pg_ctl`/`pg_isready`/`postgres` — ALL four, a partial dir is rejected) plus `nats-server` on PATH, cached once per container lifetime. Base/`:gui` images skip the plane entirely — behavior identical to today. A **watchdog** (`watchSandboxPlane`, 15s interval) re-boots the stack via the idempotent `bootSandboxPlane` when `/healthz` stops answering; Postgres is restarted in place (pg_ctl reuses the stable cluster — no re-initdb), and a best-effort `pg_ctl stop` runs on supervisor exit (container teardown is the real guarantee). The **serve handshake** reports `plane_enabled`, the daemon publishes `CreateResponse.PlaneURL = http://<container-ip>:8080` on dev images (empty otherwise), and the **run-start gate** probes the plane's `/healthz` on the bridge IP alongside the serve L1 check — a half-initialized plane can't pass.

  **MCP against the sandbox:** on dev images the container serve's config registers the built-in Orchicon MCP (`orchicon mcp`) pointed at the sandbox Postgres — the entry's environment carries `ORCHICON_MCP_TENANT_ID` + `ORCHICON_POSTGRES_DSN` (the sandbox DSN), and the command is forced to `/usr/local/bin/orchicon` (the daemon's read-only bind-mount, guaranteed present; the plane's own executable path is not). Workers get the `orchicon_*` tools natively against their own disposable DB — never the host plane's. Create-to-ready is ~6-8s on first dispatch per run (initdb ~0.4s + serve boot + migrations), overlapping the existing ~4s opencode-serve warm; warm-pool checkouts that find the plane mid-boot simply wait in the run-start gate.

  **No-DB-route invariant preserved:** the sandbox plane's DSN is container-local trust auth — it never learns the host plane's DSN, and the MCP sidecar is pointed at the sandbox DSN. The plane dies with the container (pool reset recreates pristine).

**Build:** `make container-build` also builds `orchicon-runtime:local` (plus `orchicon-runtime:local-gui` and `orchicon-runtime:orchicon-dev`) — **version-gated, not unconditional**. Each stock runtime image is tagged with an `org.orchicon.runtime.version` label derived from the app version + the SHA-256 of its Dockerfile (`<version>-<sha12>`; the `:gui`/`:dev` versions embed the base's version so a base change cascades to them). On every `container-build` the script inspects the existing image's label and **skips the build when it matches** — the base image no longer bakes the orchicon binary (the daemon mounts its own at container-create time), so its content is a pure function of the Dockerfiles and a normal dev rebuild (binary changed, Dockerfiles unchanged) skips all three. The script also skips any image carrying `org.orchicon.runtime.spec-version` (a daemon-built image from an edited+redeployed canned row) — container.sh never clobbers a tenant-owned build. Touch a Dockerfile (or bump the app version) and the affected variants rebuild. Escape hatch: `FORCE_RUNTIME=1 make container-build` (or `ORCHICON_FORCE_RUNTIME_REBUILD=1 scripts/container.sh build`) rebuilds regardless. The release workflow ships the runtime image to GHCR (`ghcr.io/beardedparrott/orchicon-runtime:<version>` + `:latest`, plus the `:gui` and `:dev` variants) — the one-command install pulls it, and the runtime daemon defaults to that image (`ORCHICON_RUNTIME_IMAGE` overrides; local dev pins the locally-built tag). `ORCHICON_RUNTIME_IMAGES` adds extra allowlisted stock images (base always included). **Model note:** executions dispatch with the worker's pinned `model_ref`; verification workers should pin a free model (e.g. `opencode/deepseek-v4-flash-free`).

### Persistent Worker Sessions (session transport)

Worker executions run through a **persistent opencode session** — the ONLY execution transport (the legacy one-shot `opencode run` subprocess path was removed: a run that cannot get a session fails fast, `failed_to_start` → workflow recovery, instead of silently degrading to a second, inferior transport). One `opencode serve` instance owns each execution's session and its agent loop; the goal is the first user message, and liveness nudges + mid-run human messages join the session's per-session prompt queue (serialized by the server, so an injected message lands at the next turn boundary).

**Serve topology** (two populations, one `SessionClient` — `internal/opencode/session.go`):
- **Always-on host serve** (`internal/opencode/servehost.go`) for the in-process population — standalone dispatches, follow-ups, Ask Orchicon later, and any execution not bound to a workflow run. Spawned by the control plane at boot, supervised by a health watchdog with restart + backoff, against a **dedicated** persistent data dir (`~/.local/share/orchicon/opencode`, seeded with the operator's model auth — never shares an `opencode.db` with the operator's own opencode). Sessions survive serve restarts. The operator's MCP servers are merged in, plus the built-in Orchicon MCP (tenant-scoped).
- **Per-workflow-run container serve** for workflow executions. The supervisor starts `opencode serve` as a detached child (0.0.0.0, fixed port 4096, same guard + **stable** XDG data dir `/tmp/orchicon-serve-data`), owns the serve password (idempotent handshake), and the daemon returns the plane-reachable base URL (`http://<container-ip>:4096`) in `CreateResponse.ServeURL`. The serve is **warmed at container-create time** (the run-start gate passes `ServeConfig` to the daemon's checkout), so dispatch never cold-starts it. The plane reaches the serve **directly on the docker bridge** — no published port, no docker-proxy (a containerized plane cannot reach the host loopback, and published-port forwarding to a serve that starts lazily races). The serve config omits the operator's MCP servers (`SkipUserMCP`): a serve eagerly connects to every configured MCP server at startup, and the `orchicon`/`orchicon-dev` entries (which `docker exec` into containers) would hang it.
 - **Serve watchdog** (`internal/runtime/agent.go` `watchServe`): the supervisor polls the container serve every 10s and restarts it (in place, same port + password + stable XDG data dir, so sessions survive and the SSE client re-attaches by id) when it stops answering `/global/health` — covering a serve that WEDGES (alive but hung), which `watchExec` (process-exit only) never sees. The `runServe` idempotent handshake is liveness-gated: a registered-but-unhealthy serve is reported as DOWN (and restarted) rather than "up", so a dispatch never burns its 30s readiness probe against a dead serve.
 - **Run-start serve gate** (`runtime_ready`, `internal/runtime/lifecycle.go` `EnsureServing`): a workflow run does NOT dispatch **any** execution until its runtime container's opencode serve is **proven usable** (L1: `/global/health` AND a real session-create round-trip). The pending→running transition persists `runtime_ready=false`; an async ensure-serving pass (one goroutine per run, idempotent, re-triggered after a plane restart) leases the container, warms + probes the serve within `ORCHICON_RUNTIME_SERVE_READY_TIMEOUT` (default 120s), and flips the flag; the `WorkflowReconciler` holds step-DAG progression (and the `TaskReconciler` belt-and-suspenders-skips dispatch) while it is false. On `:orchicon-dev` images the gate ALSO requires the **sandbox plane's** `/healthz` to answer on the container bridge IP (inside the same window) — a half-initialized plane can't pass, so no execution dispatches against a run whose sandbox environment isn't ready. This converts the old dispatch-time race — a cold-starting serve failing the first execution's 30s window, then recovery looping — into a deterministic check at run start. A serve that cannot come up **fails the run at start** with a clear error (+ container release) instead of a step-level recovery loop. Headless serve sets the flag true immediately.
 - **Model-layer wedge recycling** (adapter, `internal/opencode/session_run.go`): the serve health watchdog cannot see a serve whose `/global/health` answers but whose **model turns fail instantly** (provider/API-level `session.error`). When `ORCHICON_SESSION_ERROR_RECYCLE_THRESHOLD` (default `3`) consecutive session errors accumulate across executions, the adapter **recycles the affected workflow's runtime container** (`Kill` → the lease is released + the container reset in the background; the next dispatch's checkout builds a fresh serve) — exactly the manual fix that un-wedged a field incident where every auto-retry re-hammered a poisoned serve in ~80ms. Any non-error progress (step/tool/message) resets the counter, so a single transient failure never recycles; the env override is documented in Settings → Defaults.
 - **Fail-fast dispatch**: `sessionClientFor` returning nil (no host serve, or the daemon's checkout failing to bring the container serve up) is a hard error → `failed_to_start` → workflow recovery. The daemon propagates serve-start errors instead of silently degrading; with the run-start gate, dispatch never races a cold serve.

**Transport:** `SessionClient` (create / `prompt_async` / abort / permission auto-reply) + the server's `/event` SSE bus, mapped to the same `{type, part}` legacy events `opencode run --format json` emits (`legacyEventFromBus` mirrors `run.ts`) and fed into the unchanged `parseEvent` pipeline — stall monitor, usage recorder, artifacts, summary accumulation, streaming callbacks all work identically across both transports. Completion is driven by `session.idle` (the server emits it only when every queued prompt is answered — a single user message spans many steps/tool loops, so step-finish alone is not a turn boundary). Worker system prompts ride the per-message `system` field (opencode applies it per turn), so a shared serve hosts different workers; the `ORCHICON WORKER SUMMARY: success|failure` decision-signal contract is preserved.

**Guardrails:** fatal stalls (`no_progress`/`text_loop`/`repetition`) and the wall-clock backstop abort the session (`POST /session/:id/abort`) instead of killing a process (the serve is shared). The advisory `no_file_progress` stall now sends a **liveness probe** — a `prompt_async` message asking the worker to report status and continue; any post-probe activity or a completed turn is evidence of liveness and **revives** the execution (`recovered:liveness_probe`, clearing the `stalled` notice), while no activity in the reply window fails it (`liveness_probe_no_response`). A **completion probe** closes the mirror-image hole at the END of a run: when `session.idle` fires but the accumulated output carries no `ORCHICON WORKER SUMMARY:` marker — the signature of a final model turn truncated mid-stream (a `step_finish` with reason `unknown`/0 tokens, so the worker never delivered its decision signal) — the still-live session is re-prompted to finish the summary instead of being recorded as a hollow success; a reply carrying the marker settles normally, and a session that still cannot produce it after the probe budget fails with `stalled:missing_decision_signal:completion_probe_no_response` so the workflow's loop-decision/re-ask/fail path (not a phantom `succeeded`) owns the missing signal. The stream-structure check was likewise hardened: a `step_finish` with reason `unknown` and zero tokens now counts as an unfinished final turn (`stats.unfinished()`), so the `stepStarts > stepFinishes` balance check no longer lets a 38/38-but-truncated stream slip through as a clean success (the downgrade is skipped if the output still carries the decision marker — the probe may have salvaged it). Caps: `ORCHICON_STALL_NUDGE_MAX` (default 2), one per advisory window, `ORCHICON_STALL_NUDGE_REPLY_WINDOW` (default 300s), `ORCHICON_STALL_NUDGE_COOLDOWN`. This resolves the false-positive class (an analyst producing output but not touching files no longer trips an unresolvable notice). The global kill-switch `ORCHICON_OPCODE_SESSION_TRANSPORT=0` and any serve-unavailable condition now make the execution FAIL FAST (no legacy one-shot fallback — that path was removed).

**Durable transcript + mid-run chat:** every session event (goal, nudges, human messages, assistant text, tool calls, reasoning, steps, errors) is recorded into `execution_session_parts` (tenant-scoped RLS, `ON DELETE CASCADE` from the execution) — the durable record that survives the serve/container lifecycle and is kept forever. The execution detail page renders it as an Ask-Orchicon-grade chat (the **SessionChatPane**): user messages right / assistant left, collapsible tool cards, auto-stick-to-bottom scrolling, and a composer that injects a mid-run message via `SendExecutionMessage` (no new execution/work item/workflow state — the reply streams back through the normal event stream). On a **completed** execution the same composer runs a one-shot **follow-up in the session** (`ContinueExecutionSession`): it re-attaches to the original session when its serve is still reachable, else seeds a fresh host-serve session with the durable transcript as context, and records the question + reply inline in the transcript — the conversation continues naturally, with **no new execution or work item**. The follow-up is **fire-and-forget**: the RPC records the user's question synchronously (the chat shows the bubble immediately) and returns at once, while the reply is collected asynchronously on a request-independent context (`context.WithoutCancel`) and appended when it lands — a long model turn can never block the browser connection (which previously surfaced as `NetworkError when attempting to fetch resource` on browsers with a response timeout, e.g. Firefox's ~115s default) nor discard the reply when the client disconnects mid-turn. The SessionChatPane polls the transcript while a follow-up reply is pending so the assistant's answer appears without a manual refresh; the reply window is `ORCHICON_FOLLOWUP_REPLY_WINDOW` (default 30m). `GetExecutionSession` returns the transcript for history.

`WorkerExecution.worker_name` (proto field 25) carries the worker's display name, LEFT JOINed from `workers` (tenant-scoped) by every execution reader — `GetExecution`, `ListExecutions`, `ListDispatchingExecutions`, `ListRunningExecutions`, `GetLatestExecutionForTask`. The executions list, execution detail, and run-detail pages render `worker_name` and fall back to the raw `worker_id` only when the worker row is gone (deleted); the UI derives no display name from an ID convention.

### Manual Development Setup

```bash
# Terminal 1: Full stack (single container)scripts/container.sh up dev           # Postgres, NATS, OTel, Tempo, Loki, VM, Grafana + control plane

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
| **Hygiene** | |
| `clean` | Clear the Go build cache (`go clean -cache -testcache -modcache`) + `bin/` |
| `cache-check` | Report the current Go build cache size |
| `clean-docker` | Prune dangling Docker images + stopped containers + unused volumes (keeps live instances + data volumes) |
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
- **CSS**: Tailwind utility classes with 28 CSS-variable themes (14 light + 14 dark)
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
| `ORCHICON_DEPLOYMENT_TENANT_ID` | `tnt_dev` | The single tenant this deployment owns: OIDC callback, dev-login, embedded-OP local login, and the local-admin bootstrap all resolve logins into it; boot provisions it via the tenant seed. Must be lowercase alphanumerics plus `-`/`_` (≤63 chars); a misconfigured value fails boot. IdP identity claims are never consulted for tenant selection |
| `ORCHICON_BLOB_STORE` | `local` | Blob store backend: `local` or `s3` |
| `ORCHICON_OIDC_ISSUER` | `local` | OIDC issuer URL (or `local` for dev IdP) |
| `ORCHICON_OIDC_CLIENT_ID` | (none) | OIDC client ID |
| `ORCHICON_OIDC_CLIENT_SECRET` | (none) | OIDC client secret |
| `ORCHICON_SIGNING_KEY` | (auto-generated) | JWT signing key (required in production) |
| `ORCHICON_SIMULATE_ADAPTER` | `false` | Enable adapter simulation mode (no-op dispatch) |
| `ORCHICON_STALL_NO_PROGRESS_WINDOW` | `300s` | Time without step_finish/token progress before stall (overrides DB setting) |
| `ORCHICON_STALL_NO_FILE_DIFF_WINDOW` | `15m` | Time without file modifications before stall — **advisory only**; the execution is NOT failed. A reviewer/QA worker may legitimately produce output without touching files (overrides DB setting) |
| `ORCHICON_STALL_TEXT_LOOP_WINDOW` | `10m` | Time of text-only output with no meaningful action before stall (overrides DB setting) |
| `ORCHICON_STALL_REPETITION_COUNT` | `5` | Repeated tool calls before stall within window (overrides DB setting) |
| `ORCHICON_STALL_REPETITION_WINDOW` | `300s` | Window for repetition count detection (overrides DB setting) |
| `ORCHICON_STALL_WALL_CLOCK_SECONDS` | `3600` | Hard per-execution timeout in seconds. 0 = disabled. Default 3600 (1 hour). (overrides DB setting) |
| `ORCHICON_OPCODE_SESSION_TRANSPORT` | `1` | Session transport master switch. `0` = session transport disabled → every execution FAILS fast (the one-shot `opencode run` fallback was removed) |
| `ORCHICON_STALL_NUDGE_MAX` | `2` | Max liveness probes per execution (advisory no_file_progress stalls) |
| `ORCHICON_STALL_NUDGE_REPLY_WINDOW` | `300s` | Probe reply window; no activity within it fails the execution |
| `ORCHICON_STALL_NUDGE_COOLDOWN` | `60s` | Minimum gap between probes |
| `ORCHICON_FOLLOWUP_REPLY_WINDOW` | `30m` | Async follow-up reply window; the collected reply is written when it lands within this bound |
| `ORCHICON_ASK_REPLY_WINDOW` | `30m` | Ask Orchicon detached reply window; a turn that does not complete within this bound is persisted as a timeout error message |
| `ORCHICON_ASK_TIMEOUT` | `60s` | Ask Orchicon send-accept bound: how long a turn's attempt waits for the serve to accept the sent message once subscribed. A wedged serve fails fast instead of silently queuing |
| `ORCHICON_ASK_SERVE_DOWN_GRACE` | `15s` | Ask Orchicon serve-down fast-fail: how long a turn whose serve has NEVER accepted a connection keeps retrying before the reply is persisted as a serve-unavailable error (a serve that was live earlier keeps the full reply window for restart recovery) |
| `ORCHICON_ASK_REATTACH_BACKOFF` | `2s` | Ask Orchicon pause between re-attach attempts after serve loss mid-reply (the collector retries inside the reply window) |
| `ORCHICON_ASK_STALL_NO_PROGRESS_WINDOW` | `120s` | Ask Orchicon stall monitor: no text/reasoning/step_finish/tool_use for our session within the window trips `no_progress`, aborts the serve session and fails the turn with a clear, retryable error |
| `ORCHICON_ASK_STALL_REPETITION_COUNT` | `5` | Ask Orchicon stall monitor: same tool-call signature repeated more than this many times within the repetition window trips `repetition` |
| `ORCHICON_ASK_STALL_REPETITION_WINDOW` | `300s` | Ask Orchicon stall monitor: the window over which identical tool-call signatures are counted for the repetition signal |
| `ORCHICON_ASK_TURN_MAX_AGE` | `31m` | Ask Orchicon turn-registry TTL: a turn older than this is evicted by the background sweeper (collector cancelled, serve session aborted) so no conversation can be blocked forever by a wedged collector |
| `ORCHICON_ASK_SWEEP_INTERVAL` | `1m` | Ask Orchicon turn-registry sweeper tick interval (dev/test knob) |
| `ORCHICON_MCP_TENANT_ID` | `tnt_dev` | Tenant for the built-in Orchicon MCP registered on the host serve |
| `ORCHICON_REAP_GRACE_SECONDS` | `60` | Liveness reaper: min execution age before reaping is considered (overrides DB setting) |
| `ORCHICON_REAP_CONSECUTIVE_FAILURES` | `3` | Liveness reaper: consecutive not-alive probes before an execution is reaped (overrides DB setting) |
| `ORCHICON_SESSION_ERROR_RECYCLE_THRESHOLD` | `3` | Session-transport watchdog: consecutive model-layer `session.error` failures before the adapter recycles the workflow's runtime container (a serve whose health answers but whose model turns fail — invisible to the health watchdog). `<1` disables recycling |
| `ORCHICON_RECONNECT_ATTEMPTS` | `3` | Transport resilience: client retries of a broken exec stream (overrides DB setting) |
| `ORCHICON_RECONNECT_GRACE_SECONDS` | `60` | Transport resilience: supervisor keep-alive for an orphaned child before killing it (overrides DB setting) |
| `ORCHICON_LOG_DIR` | `.dev/logs` | Directory for the rotating serve log file (detached `serve --detach`) |
| `ORCHICON_LOG_MAX_SIZE_MB` | `100` | Rotate the active log file once it exceeds this size (MB) |
| `ORCHICON_LOG_ROLL_INTERVAL_HOURS` | `24` | Rotate by time at least this often (hours; 24 = daily, 1 = hourly) |
| `ORCHICON_LOG_RETENTION_DAYS` | `7` | Prune rotated log files older than this many days |
| `ORCHICON_LOG_MAX_FILES` | `7` | Keep at most this many rotated log files (newest kept) |
| `ORCHICON_RUNTIME_MAX_AGE` | `24h` | **Obsolete** — the warm pool owns container cleanup (reset at daemon start + idle-reap). Kept for config compatibility; no longer read. |
| `ORCHICON_RUNTIME_SWEEP_INTERVAL` | `5m` | **Obsolete** — superseded by the pool's idle-reap. Kept for config compatibility; no longer read. |
| `ORCHICON_RUNTIME_SERVE_READY_TIMEOUT` | `120s` | Run-start serve gate: how long the async ensure-serving pass probes the runtime container's opencode serve (L1: health + session-create) before failing the run at start. |
| `ORCHICON_RUNTIME_POOL_CAP` | `1` | Warm pool: max clean (idle) containers kept per environment (image + project mounts). |
| `ORCHICON_RUNTIME_POOL_IDLE` | `10m` | Warm pool: how long a clean container may sit unused before it is idle-reaped. |
| `ORCHICON_INDEX_CHECK_INTERVAL` | `6h` | Control plane: how often the amcheck index-integrity sweep runs (0 = boot check only). A corrupted btree index silently hides rows from `=` lookups; the sweep validates every user btree index and rebuilds corrupt ones with `REINDEX INDEX CONCURRENTLY` |

---

## Log Management (Rotating Serve Logs)

`orchicon serve --detach` writes its structured log to a single rotating
file (default `.dev/logs/orchicon.log`). The serve child owns the file
and rotates it **by size** (when it exceeds the size ceiling) **or by
time** (whichever comes first), then **prunes** old rotated files by
retention age and a maximum file count. A run-away component can no
longer grow an unbounded single log file — the file is always capped at
`max_size` and only `retention_days`/`max_files` of history are kept.

The rotated files are siblings named `orchicon.log.<timestamp>` in the
same directory. The serve child also dup2's the current log file onto
fds 1/2, so panics and stray prints (which bypass slog) land in the
current log, and re-points those fds after each rotation.

Configuration precedence (per field):

1. **Settings → Defaults → Log management** (tenant DB) — live-applied to
   a running detached serve every ~5s, no restart needed.
2. **`ORCHICON_LOG_*` env vars** — dev overrides.
3. **Built-in defaults** — 100 MB max size, 24h roll, 7 days retention,
   7 files.

In the single-container deployment the control plane's stdout goes to
Docker's `json-file` log driver, which is bounded by the
`--log-opt max-size=100m` / `max-file=7` flags set on the instance
containers (`scripts/container.sh` and `orchicon install`).

## Container Image Hygiene

Repeated local builds of the four tagged images (`orchicon:local`,
`orchicon-runtime:local`, `:local-gui`, `:orchicon-dev`) and custom
runtime-image builds orphan the previous image as a dangling layer.
Both `scripts/container.sh build` and the runtime daemon's image-build
path prune dangling images after each build. To reclaim space from
pre-existing orphans or the Go build cache:

```bash
make clean-docker                                   # prune dangling images + stopped containers + unused volumes
make clean                                          # clear the Go build cache (go clean -cache -testcache -modcache)
make cache-check                                    # report the current Go build cache size
```

`make clean` only touches the Go build cache and `bin/`; it never
touches the database, container images, or runtime data.
`make clean-docker` prunes dangling images, stopped containers, and
volumes referenced by no container — the live instance containers
(dev/prod), their data volumes, and the Postgres volumes that preserve
instance data are always kept.

### Telemetry data retention

The embedded telemetry backends prune their data so the instance data
volume cannot grow unbounded:

| Backend | Retention | Config |
|---|---|---|
| Loki (logs) | 14 days | `retention_period: 336h` + compactor (`retention_enabled`, `delete_request_store: filesystem`) in `deploy/container/configs/loki.yaml` |
| Tempo (traces) | 14 days | `compactor.compaction.block_retention: 336h` in `deploy/container/configs/tempo.yaml` |
| VictoriaMetrics (metrics) | 30 days | `-retentionPeriod=720h` flag in `cmd/orchicon/container.go` |

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

### Admin Access & Lockout Recovery

The plane guarantees there is **always at least one admin path in** (AC of
the first-boot admin bootstrap): a fresh install seeds an admin for the
embedded-OP login, and a locked-out operator can always re-arm the credential
at boot.

| Task | How |
|---|---|
| Find the first-boot admin password | Read the boot log line `local-mode bootstrap admin created: username ..., password ...` (shown once on first boot; only when unpinned — `ORCHICON_LOCAL_ADMIN_PASSWORD` skips it). |
| Change the admin password (you are logged in) | **Admin → Identities → "Set local password"**: pick the identity, set username + new password. This calls the admin-only `SetLocalCredential` RPC (`auth:write`); the password is argon2id-hashed at the boundary and the hash is never returned. Equivalent API call: `POST /orchicon.api.v1.AuthService/SetLocalCredential` with `{identity_id, username, password}`. |
| Lost the admin password (locked out) | Stop the plane, set `ORCHICON_LOCAL_ADMIN_RESET=1` (pin the new one with `ORCHICON_LOCAL_ADMIN_PASSWORD`, or leave it unset for a random one logged once), start it again, sign in with the new credential, then unset the env so the next boot is normal. The reset overwrites only the credential — the admin identity and role binding are preserved. It is local mode + embedded OP only and works even when `ORCHICON_LOCAL_ADMIN_SEED=0`, so a locked-out operator always has a path back in. |
| No admin was ever seeded | Either the plane is in production mode (external IdP owns auth) or `ORCHICON_LOCAL_ADMIN_SEED=0` was set. In local mode with the embedded OP enabled, set `ORCHICON_LOCAL_ADMIN_RESET=1` to force the seed to run. |
| Dev escape hatch | `ORCHICON_DEV_LOGIN=true` (local mode only) enables the synthetic `/auth/dev-login` — a dev convenience, not the auth path; the prod dogfooding container runs it disabled. |

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
| Execution page shows the wrong system prompt (e.g. every worker looks like the first step's role) | Page read the shared work item's `prompt_context` | The work item is a shared input reference whose `prompt_context` carries the FIRST step's composite and never changes. Since v0.1.187 the execution page shows `WorkerExecution.system_prompt`, resolved from the linked workflow step run's `_prompt` (the actual per-step composite the model received). |
| Loop decision stuck | Superseded step run conflict | Workflow reconciler must skip `SupersededBy != ""` runs |
| Workflow run stuck "running" though the execution succeeded | A reconcile pass errored on a LATER step and rolled back the whole transaction | The run's step shows `running` but its `worker_executions` row is `succeeded`. Since v0.1.186 a step-dispatch failure fails that step instead of rolling back the pass; for runs wedged before that (or any other stuck state) use the **Force next step** button on the run view (`ForceProgressWorkflowRun` RPC). Force-progress marks only the STUCK step run(s) succeeded — in-flight steps and pending steps whose DAG deps are already satisfied — and leaves steps still waiting on an unresolved upstream PENDING, so the reconciler's next pass dispatches them normally (the loop DAG keeps progressing; it never skips real downstream work like a PR-merge step). A run can also wedge when a SUPERSEDED loop-decision iteration shadows the active one in the reconciler's `runByID` map (same-transaction iterations share a `created_at`): `ListWorkflowStepRuns` now orders by `created_at, id` and the map prefers non-superseded rows, and the upstream-failed branch no longer spawns duplicate iterations while one is already pending. Also check the indexes: a corrupted btree index hides rows from `=` lookups (see `ORCHICON_INDEX_CHECK_INTERVAL`); the control plane now sweeps + auto-rebuilds them. |
| Workflow run failed and you want to resume it | A step hit its terminal failure | Use the **Retry failed step** button on the failed run's view (`RetryFailedWorkflowRun` RPC). It resets the run to `pending` (clearing its ended timestamp), re-arms every active failed/skipped/blocked step run as `pending` (clearing result, worker execution ref, attempt, and ended timestamp so the reconciler re-dispatches it), flips the bound work item back to `running`, and the reconciler re-creates the runtime container and resumes the DAG from where it left off. Steps that already succeeded are kept. |
| Control plane CPU pegged (~150%) with no DB activity | Work-queue `dequeue` busy-loop | Fixed in v0.1.186 — dequeue is bounded to one rotation pass. Restart the control plane to clear a wedged reconcile goroutine (its advisory lock never renews while stuck). |
| Stale decisions leaking across runs | Previous `_decision` file | Clear `.orchicon/<run_id>/` files between steps |
| Worker cannot delete or run destructive commands | Sandbox layers | Workers are intentionally sandboxed (see Architectural Pattern 8). Direct bash is blocked by opencode permission deny rules; subprocess/TUI-issued commands (e.g. `rm -rf /` inside a python TUI) are blocked by the OS-level execution guard (`internal/guard/guard.go`, built inside the workflow runtime container); and all canned workers' prompts carry the "Safety rules" block. Review/QA workers run `semgrep scan --config .orchicon/semgrep_orchicon.yml --error .` (Semgrep + Orchicon ruleset) to catch dangerous patterns before merge. |
| Worker wiped files outside the project | Execution guard bypassed via absolute path | The guard is defense-in-depth, not containment. A worker that invokes `/bin/rm` by absolute path or writes its own binary escapes it. Run Orchicon as the single-container deployment (§Single-Container Deployment) for a real process-isolation boundary. |

---

## Contributing

### Branch Workflow

`develop` is the integration branch; `main` is release-only. All workers
branch off `develop`, PR into `develop`, and merge into `develop` — never
`main`. The human tests the accumulated `develop` state and approves a
`develop` → `main` merge to cut a release (per-PR releases do not happen).

1. Never commit to `main` or `develop` — the pre-commit hook enforces this
2. Branch off `develop`: `git switch -c <type>/<short-description> develop`
3. Before starting work: `git tag -a v0.1.<next> -m "v0.1.<next>"` then bump version
4. Commit early and often with clear present-tense messages
5. Before PR: update `UPDATES.md` (leave README.md's "Last Release Changes"
   section alone — it only changes when the human cuts a release)
6. Ask for approval before creating a PR
7. PRs target `develop` and must NOT carry the `release` label (that label
   belongs only on the human's `develop` → `main` release PR; merging into
   `develop` never creates a release)

### Local Pre-commit Hook

```bash
#!/bin/sh
# .git/hooks/pre-commit
branch="$(git symbolic-ref --short HEAD)"
if [ "$branch" = "main" ] || [ "$branch" = "master" ] || [ "$branch" = "develop" ]; then
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

### Third-party notices

The binary embeds the Apache-2.0 license + notice for the vendored
`github.com/zitadel/oidc/v3` library (`third_party/oidc/`), so the license
obligation ships in the distribution. Print them with:

```bash
orchicon notices
```
