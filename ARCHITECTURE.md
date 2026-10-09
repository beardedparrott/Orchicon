# Orchicon — Architecture & Development

> **Orchicon** is an AI orchestration and operations platform. It coordinates autonomous AI work as reliable, observable, recoverable, and manageable systems by separating **orchestration** from **execution**. The control plane manages projects, workers, scheduling, policies, telemetry, recovery, and governance, while pluggable runtimes execute the actual work.

> **Using Orchicon, rather than building it?** See [`USERGUIDE.md`](./USERGUIDE.md) — installation, first run, and a screen-by-screen guide to both the web GUI and the terminal client. **This document** covers how Orchicon is built: architecture, the data model, the Ask Orchicon permission model, operator setup, development, deployment and troubleshooting.

---

## Table of Contents

1. [Project Overview](#project-overview)
2. [Technologies Used](#technologies-used)
3. [Overall Architecture](#overall-architecture)
4. [Project Structure](#project-structure)
5. [Ask Orchicon & Permissions](#ask-orchicon--permissions)
6. [Operator Setup (Adapters)](#operator-setup-adapters)
7. [Development Guide](#development-guide)
8. [Deployment](#deployment)
9. [Environment Variables Reference](#environment-variables-reference)
10. [Log Management (Rotating Serve Logs)](#log-management-rotating-serve-logs)
11. [Container Image Hygiene](#container-image-hygiene)
12. [Troubleshooting](#troubleshooting)
13. [Contributing](#contributing)
14. [License](#license)

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
| Runtime Adapter | **Built-in native engine (default)**, plus external adapters (OpenCode, …) | Sessions run inside the control plane by default, so **no adapter CLI is required**. External adapters are pluggable over gRPC and mounted from the operator's host install when a run asks for one — never bundled |
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
        AdapterBridge[Adapter Bridges<br/>native engine + external adapters]
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
        NativeEngine[orchicon native engine<br/>in-process · default]
        OpenCode[OpenCode CLI<br/>optional · inside runtime container]
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
    AdapterBridge --> NativeEngine
    AdapterBridge --> OpenCode
    OutboxRelay --> NATS
    NATS --> WebhookDispatch
    WebhookDispatch --> HTTP
    AdapterBridge --> RuntimeDaemon
    RuntimeDaemon --> RuntimeContainer
    RuntimeContainer --> OpenCode
    AdapterBridge -.-> FutureRuntime
    AIGateway -.-> OpenCode
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

1. **Kubernetes-style Reconcilers** — Six reconcilers (Task, Workflow, Recovery, ScheduledRun, Sequence, RecurringFire) run in a shared manager with per-kind PostgreSQL advisory locks for leader election. Each has a work queue with exponential backoff and a scan pass for discovering work. The work-queue `dequeue` is bounded to one rotation pass (a single not-ready key returns `ok=false` instead of busy-looping — a field incident pinned a core at ~150% CPU and froze the reconciler), and the workflow DAG-progression loop is capped (`maxDAGPasses`) so a pathological run can never wedge a reconcile goroutine — a capped pass now `break`s and commits its accumulated progress rather than erroring, so a step's staged recovery (`recovering` transition + pending `TriggerOnFailure`) isn't silently rolled back and lost. A step-dispatch failure that can't be resolved (e.g. a missing/corrupted worker-version lookup) fails that **step** rather than erroring the whole pass, so completed upstream steps are not rolled back with it.

2. **Transactional Outbox** — Every mutation writes an outbox row in the same database transaction as the state change. A background relay polls unpublished rows every 500ms and publishes to NATS JetStream for at-least-once delivery.

3. **Single Binary** — The Go binary embeds the single-container runtime configs (`deploy/container/configs/`), SQL migrations, and the built frontend SPA via `go:embed`. No external dependencies at runtime beyond Docker. The **single container** (`orchicon container` runs the whole stack as PID-1 — §Single-Container Deployment) is the only full-stack deployment; the same binary also runs headless via `orchicon serve`.

4. **Non-blocking OTel** — The OpenTelemetry pipeline uses `grpc.NewClient` (non-blocking dial), so the control plane boots in <2 seconds even when the OTel collector is not yet healthy.

5. **Connect-ES** — Single Protobuf schema generates both Go server and TypeScript client code. Supports unary RPC, server-streaming, and client-streaming over the same interface.

6. **RLS-backed Tenant Isolation** — Every tenant-scoped table has a PostgreSQL Row-Level Security policy as a backstop. The data-access layer also injects `app.tenant_id` via session variables.

7. **Adapter Bridge Pattern** — Runtimes are pluggable gRPC sidecars. The built-in adapter drives the OpenCode CLI — locally as a subprocess (headless `orchicon serve`) or, for workflow-run executions, inside the per-workflow runtime container via `orchicon runtime-daemon` — parsing its JSON telemetry output. Future runtimes implement the `orchicon.adapter.v1` gRPC contract.

8. **Worker Sandboxing (layered defense)** — Every worker execution is contained by three layers, all applied to **every** worker automatically and enforced even under `--auto`:
    - **opencode permission deny rules** (`permissionRules()` in `internal/opencode/config.go`) injected via `OPENCODE_CONFIG_CONTENT`. `external_directory` is `deny` by default with a **single precise carve-out**: `/tmp/orchicon/**` is `allow`, so workers can use `/tmp/orchicon` as a scratch directory (screenshots, logs, downloaded artifacts) but every other path outside the project's `--dir` is blocked — the carve-out deliberately does not match the supervisor socket, the execution-guard shims, or the `/tmp/opencode-data-*` dirs that hold the seeded model auth.json copies. An extensive `bash` deny list blocks `rm`/`sudo`/`dd`/`mkfs*`/`fdisk`/`parted`/`shred`/`wipefs`/LVM tools, root-wide `chmod -R`/`chown -R`, `/dev/sd*` redirection, shell-construct smuggling variants (`(rm -rf /) &`, `{ rm -rf /; }`, chained `;`/`&&`/`&`/`|`), and download-and-execute. No catch-all `*` allow rule is emitted. There are now **two permission profiles**, selected by `ConfigOptions.PermissionProfile` (the zero value `""` means **worker**): the **worker** profile above, carried byte-identically by every dispatched execution and by the shared worker serve, and an **interactive** profile carried by the Ask Orchicon serve (`NewAskHostServe` — its own `opencode serve`, because the permission config is per-process) — `external_directory` **allowed** (an interactive session reaches the operator's own filesystem; the tool-layer boundary and the consent layer decide scope and action), writes and commands raising opencode's own `ask` (which is what makes the `permission.asked` events we relay real asks rather than an artifact of deny-by-default; the ask's detail now rides `scheduler.SessionEvent.Detail` to the consent layer), and the never-allow class still hard-denied. The interactive profile deliberately does **not** inherit the composite-tools `read`/`grep` deny — that exists to force workers onto `batch_read`/`batch_grep` and is worker-only. The never-allow class itself is declared once in `internal/neverallow` and consumed by BOTH the config builder and the OS-level execution guard (`internal/guard`, which renders its shim set and its `case` arm from it), so the two layers cannot drift apart.
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
        string id PK "ULID"
        string name
    }
    Project {
        string id PK "ULID"
        string tenant_id
        string name
        string slug
        string status
        json goals
    }
    WorkItem {
        string id PK "ULID"
        string tenant_id
        string project_id
        string kind "Epic|Feature|Task|Subtask"
        string status "pending|ready|assigned|running|checkpointing|succeeded|failed|cancelled|recovering|scheduled|recurring|blocked"
        string assigned_worker_ref
        json recurring_schedule "NULL or {frequency, interval, days[], start_date, start_time}"
        timestamp next_run_at "computed next occurrence of a recurring item"
    }
    Worker {
        string id PK "ULID"
        string tenant_id
        string project_id
        string status "draft|published|deprecated|retired"
        json model_ref
        json budget_overrides
    }
    WorkerExecution {
        string id PK "ULID"
        string tenant_id
        string work_item_id
        string worker_id
        string status "pending|dispatching|running|success|failure|cancelled"
        json prompt_context
    }
    Workflow {
        string id PK "ULID"
        string tenant_id
        string project_id
        string type "one_shot|template"
    }
    WorkflowRun {
        string id PK "ULID"
        string workflow_version_id
        string status "pending|running|completed|failed"
        string work_item_id
    }
    Policy {
        string id PK "ULID"
        string tenant_id
        string project_id
        string rego_module
    }
```

---

## Project Structure

```
Orchicon/
├── assets.go                    # go:embed: container configs, migrations, frontend
├── buf.gen.yaml                 # Buf codegen config (Go + TypeScript)
├── buf.yaml                     # Buf lint config
├── ARCHITECTURE.md              # ← This file: architecture, development, deployment, ops
├── USERGUIDE.md                 # Operator's guide: install + every screen in the GUI and TUI
├── LICENSE                      # Custom license (non-commercial)
├── Makefile                     # All targets: build, test, gen, container-*, ci
├── opencode.jsonc               # Opencode tool configuration
├── README.md                    # Project introduction & quick start
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
│       ├── index.css             # Global styles + 20 themes (Tailwind + CSS vars)
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
│   ├── orchicon.sh              # Host-resident plane: start/stop/restart/status/logs/rebuild (dev|prod)
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
| **User guide** (install + every GUI/TUI screen) | `USERGUIDE.md` |
| **Frontend API clients** | `frontend/src/api/clients.ts` |
| **Frontend routes** | `frontend/src/routes/` |
| **Workflow canvas editor** | `frontend/src/components/workflow-editor/` |
| **Landing page** | `site/index.html` |
| **Install scripts** | `scripts/install.sh` (Linux/macOS), `scripts/install.ps1` (Windows → provisions WSL2, installs the Linux binary inside the distro) |
| **Container instance controller** | `scripts/container.sh` |
| **Host plane controller** | `scripts/orchicon.sh` |
| **CI/CD workflows** | `.github/workflows/` |
| **Operator's working notes** | `AGENTS.md`, `UPDATES.md`, `worker.md`, `developer.md`, `CLOUDFLARE_SETUP.md` — the maintainer's own files. **Gitignored and not in the repository**: they describe how this project is built rather than what it does, and nothing in the build, the tests or the runtime reads them from disk. (References to an "AGENTS.md" elsewhere in these docs are the per-worker *prompt field* — a database-backed section of the composed worker prompt — which is a different thing.) |

---

## Ask Orchicon & Permissions

Ask Orchicon is the conversational surface of the platform — a chat that can plan, investigate, and
act on the operator's own machine. It is the one surface where a model runs tools against *your*
filesystem, so its permission model is the most load-bearing part of it.

### The two clients

Both clients drive the **same** server, the same conversations, and the same consent decisions:

| | |
|---|---|
| **GUI** | the `/ask-orchicon` route; composer with the model chip, the stat strip, a **Fullsend** dropdown, and the mode dropdown |
| **TUI** | `bin/orch`, `F1 · Ask Orchicon`; composer with the mode pill and the `FULLSEND` badge, slash commands (`/fullsend`, `/mode`, `/grants`, `/permissions`) |

A conversation is per-project, carries its own model, and is reachable from either client. Consent
state is server-side, so a decision made in one client settles the card in the other — including a
second tab, another device, or the same page after a reload (see *Card lifecycle* below).

**The composer's hint line is the live key reference**: the TUI advertises the ACTIVE pane's chords on
the row above the input (it changes as you move between panes), so it is the surface to trust for what
a key does where you are. The chords below are the ones readers ask about, and the workflow lifecycle
is listed in full because its keys are otherwise only discoverable from that line:

| Where | Keys |
|---|---|
| **Anywhere** | `ctrl+g` focus the composer · `ctrl+d` diff rail · `enter` send · `alt+enter` newline · `/` command palette |
| **Workflows pane** | `e` edit the flow (steps) · **`V` create the next version** (a draft) · `n` new workflow · `E` rename · `p` publish · `u` deprecate · `C` categorize · `ctrl+x` delete · `space` mark for bulk · `enter` flow view · `r` refresh |
| **In the flow editor** | `↑`/`↓` move between steps · `enter` edit the selected step · `a` add a step · `x` remove a step · `E` rename the workflow · `esc` done |

A version is created as a **draft**, which is additive and reversible — so `V` does not confirm, and
neither does editing a step (editing one on a published version creates the draft for you, since
published versions are immutable). `V` exists because that implicit route was the *only* one, and a
mechanism nothing on the surface mentions is not a feature.

### The three modes

`brainstorm`, `iteration`, and `quick work` are **enforced by the platform**, not suggested in a
prompt: each is a different tool boundary (what the agent may do at all), and the boundary is applied
by the adapter so it holds for the native engine and for opencode alike. The mode is per
conversation and can be changed mid-conversation; the next message carries the new boundary. This is
why the round trip does not depend on the model honouring an instruction.

### What asks, and what never does

- **Reads never ask.** Reading a file or listing a directory cannot change anything, so it is never
  gated. A malformed permission policy does not block reads either — a broken policy must not lock
  the operator out of the file that explains the problem.
- **Writes and executions always ask**, *including inside the conversation's own project directory*.
  The project was pre-approved as a default scope and the operator removed that rung deliberately:
  *"any directory should ask before allowing on write/execute, project or otherwise."* So there is no
  silent project exemption to reason about — the only ways to stop being asked are a session grant or
  an accept entry.

### The precedence chain

Evaluated in this order, first match wins:

```
never-allow binaries  →  deny  →  session grant  →  accept  →  ask
```

- **never-allow binaries** — `sudo`, `dd`, `mkfs*`, `fdisk`, `parted`, `shred`, `wipefs`, LVM
  tooling, `mkswap`. Refused *before* the policy is consulted, so nothing can approve them: not a
  session grant, not an accept entry, not the operator, not Fullsend. They are declared once in
  `internal/neverallow` and consumed by both the opencode config builder and the OS-level execution
  guard, so the two layers cannot drift apart.
- **deny** — the operator's own exclusions. A deny is a **decision**, not a permission request: no
  card is ever raised for it, and a session grant cannot override it. The presets cover the
  credential stores an agent has no business reading (`~/.ssh`, `~/.gnupg`, `~/.aws`,
  `~/.config/gh`, `~/.git-credentials`, `~/.netrc`, `~/.docker/config.json`).
- **session grant** — "never ask again in this directory this session" (see below).
- **accept** — an entry in the policy file meaning "never prompts".
- **ask** — everything else.

The policy file is `~/.local/share/orchicon-<instance>/permission-policy.yaml` (override with
`ORCHICON_PERMISSION_POLICY`). It is read **on every gated decision**, so a hand-edit or a UI change
takes effect on the very next call — no restart, no reload.

### Session grants

Choosing *"Never ask again in &lt;directory&gt; this session"* on a card records a **grant** for that
directory:

- **It covers the subtree.** Granting `/p/proj` covers `/p/proj/internal/...` too. (It was an exact
  string match before, so granting a project root did not cover its packages and the operator got a
  card per directory — precisely the per-command friction that pushes people to approve without
  reading.)
- **Per conversation, in memory, gone on restart.** A grant is a session decision; it is never
  persisted, so a fresh plane asks again.
- **The card names the directory the grant would cover**, and that is the directory whose consent is
  actually missing — not necessarily the command's working directory. If a command's only uncovered
  path is `/tmp/x`, the card says `/tmp`, so the grant it offers is the grant that works.
- `/grants` (TUI) and the **Grants** disclosure (GUI) list the active grants and let you revoke them.

### Fullsend

**Fullsend** waives the permission *prompt* for one conversation. It exists because a gate that cannot
be opened deliberately gets bypassed accidentally: mid-task, approving card after card for the same
work, the operator stops reading them.

- **What it waives:** the ask. A write or an execution that would have raised a card proceeds.
- **What it does NOT waive:** a **deny** entry (that is a decision the policy already made — no card
  is raised for it, so there is nothing to waive) and the **never-allow class** (refused before any
  permission decision is reached). Fullsend does not open `~/.ssh`, and it does not run `sudo`.
- **Scope and lifetime:** one conversation, in memory, dying with the plane — the same scope and
  lifetime as a session grant, and for the same reason: a bypass that survives a restart is one the
  operator has forgotten is on. It is never persisted.
- **Toggleable mid-turn**, which is its primary use: nobody arms it before starting; you reach for it
  when you are already being asked too often. It takes effect on the next ask with no restart.
- **Turning it on clears a permission card already on screen**, because that card exists only because
  fullsend was off when the call was raised. A pending *question* is not cleared — a question's answer
  is the operator's own words, which the mode cannot supply.
- **Changes are audited** (`conversation.fullsend_changed`): this is the one setting whose purpose is
  to make a decision not happen, so *who opened the gate, and when* has to be reconstructible.
- **No pending form.** Fullsend applies to an open conversation; it cannot be armed for a conversation
  you have not opened.

TUI: `/fullsend` (toggles for the open conversation, and the composer shows a `FULLSEND` badge while
it is on). GUI: the **Fullsend** dropdown, immediately left of the mode dropdown.

### Card lifecycle

A card is how the turn asks, and it behaves the same in both clients:

- **It appears at the bottom of the conversation**, sectioned off with a border, and the **turn
  blocks** until it is answered. `ask_user` is genuinely blocking: the question is a pause, not a
  notification, and the operator's answer is returned as the tool result so the model resumes holding
  what they actually said.
- **A permission card offers three choices** — *Allow once*, *Never ask again in &lt;directory&gt; this
  session*, *Deny* — and a card whose target the deny list already excludes disables the session row
  (naming the entry) rather than offering a grant the policy will refuse.
- **A question card offers the model's options** plus *Other* for the operator's own words.
- **A settled card becomes a one-line record**, not a card: an answered question reads
  `You answered "<question>" — <answer>`. A card means a decision still to be made; once made it is
  history, and a full tinted block per past grant buries the live turn under its own audit trail.
- **A decision settles the card in EVERY client.** The collector writes each decision into the turn's
  ledger as a record keyed by the ask id, and the ledger is persisted with the message — so the
  transcript *is* the server's answer to "what happened to this ask", readable by any client at any
  time. The live stream event is published too, for the clients watching at that moment; the durable
  record is what makes a second tab or a reload agree.
- **A timeout is a denial, and the model is told so** — labelled as expired rather than as a refusal,
  because the operator did not refuse and the same call will ask again if retried. A malformed policy
  is reported as a policy problem, never as an operator denial.

### What a shell command's consent covers

A bash ask judges **the paths the command names**, not only its working directory — so a session grant
on `/p/proj` covers commands *run* there, while a command that touches `/etc/x` still raises a card of
its own. The extraction is deliberately modest, and honest about it:

- literal absolute paths (`/etc/x`, `--flag=/etc/x`) and `~/` paths in the command text;
- **quoted spans must look like paths** (two segments or a home prefix, no trailing slash), because a
  quoted token is usually a program or a regex — an awk program's `/^func` is not a filesystem path,
  and reading it as one produced a grant key of `/` that no grant could ever cover;
- a token containing a regex/glob metacharacter or a `$` expansion is not treated as a path;
- **heredoc bodies are skipped** (they are file content that is about to be written, not arguments),
  while a heredoc's redirect target is still judged;
- device sinks (`/dev/null`, `/dev/stdout`, …) and read-only process introspection (`/proc/self/status`,
  `/proc/&lt;pid&gt;/exe`) are not consent targets. The lists are explicit, never prefix matches: `rm -rf
  /dev/sda`, `dd of=/dev/nvme0n1`, `> /proc/sys/kernel/panic` and `> /proc/&lt;pid&gt;/mem` are all still
  judged.

A path the command *computes* (`$(cat cfg)`, a variable, a script's own logic) is invisible to this,
and the OS-level guard is what stands behind it.

**A KNOWN GAP, stated rather than glossed: a `$HOME`-spelled path is not extracted.** Only a literal
absolute path and a `~/…` path are recognised, so a command that names the same location with the
`$HOME` (or `${HOME}`) spelling is **not judged at all** — measured rather than assumed:
`echo x > ~/.config/app/conf` yields the absolute path, while the same command written with `$HOME`
yields nothing. Expansions are skipped deliberately, because a `$` token is usually a shell variable
or `$1` and reading one as a path is how invented targets came out of awk programs; that rule is what
costs this spelling. The practical consequence is narrow but real: the shimmed binaries are still
caught, and the `~/` spelling a person actually writes (as in a redirect to a credential file) is
refused, but the same target spelled with `$HOME` in a redirect is judged by neither layer — a
redirect is a shell operation, and the binary on the left of it is not one the guard shims.

### Enforcement, not just prompting

The prompt is one layer; the same rules are enforced beneath it, so approving a card is not the only
thing standing between the model and the machine:

- **OS-level execution guard** (`internal/guard`) — replaces the dangerous binaries on `PATH` with a
  shim, for **both** profiles: the one a worker execution runs under (constructed by
  `internal/runtime/agent.go` and `internal/opencode`) and the interactive Ask one. It reads the same
  policy file in both, and honours the conversation's project, session grants, once-targets and
  fullsend in the interactive profile only.
  **The deny list therefore applies to WORKER executions too** — a worker's `rm`/`cp`/`mv`/`chmod`/
  `chown`/`ln` on a path the operator denied is refused, which is the point of an exclusion the
  operator wrote. The never-allow class (`sudo`/`dd`/`mkfs*`) has always applied to both.
  **Fail-closed is the interactive profile only**: a policy file it cannot read refuses the command
  rather than running it unguarded, while a worker keeps the historical "absent policy = no policy"
  rule — so a malformed policy can never break dispatches, only the interactive surface.
- **The deny list is checked first in both layers**, and its MATCHER is spelling-agnostic about home:
  once a path reaches it, `~/.ssh`, `$HOME/.ssh` and the absolute path are the same target. What is
  NOT spelling-agnostic is the extraction that feeds it — see the `$HOME` gap under *What a shell
  command's consent covers* above. That is why this sentence names the matcher rather than claiming
  the coverage: read together they are accurate, and read apart the second one overstates.
- The **never-allow class** is a separate case arm in the shim, so no environment value can reach it.
- **A path that would DESTROY the scope is refused, not asked about** — and no approval can override
  it. `rm -rf /home` from a project, `rm -rf ~`, `rm -rf /`, and a `rm -rf` of any directory that
  *contains* the project (or a granted directory) are refused at both layers, ABOVE fullsend and above
  every allow-set, in the same position as the never-allow class and for the same reason: it is a
  decision, not a permission request. Without this the shim ran all of them once fullsend was on,
  because fullsend waives the sanctioned set by design and an ancestor of the scope is in no set to
  begin with.
  The rule is one sentence — a target is refused when it EQUALS a protected root or CONTAINS one —
  with **two lists**, because the two need different rules. **Machine roots** (`/`, the home
  directory, the plane's own state under `~/.local/share/orchicon` and `~/.orchicon`) refuse equality
  as well: there is no legitimate reason to delete `/` or to re-permission a home directory from
  inside a session. The **work scope** (the project, and each session grant) refuses *containment
  only*, because acting ON the scope root is ordinary work — `chmod -R 755 <project>` and
  `rm -rf <project>/dist` both name it — while destroying the directory that HOLDS the scope takes it
  with it and no amount of consent makes that the intent.
  The declaration lives in `internal/protectedpath` and **both layers read it**, so a consent card and
  the shim cannot disagree about what is protected.
  **Why this is not a deny entry.** A deny list matches PATTERNS against the target, so protecting the
  scope's ancestors that way would mean writing them down by hand — `/home`, `/home/<user>`,
  `/home/<user>/projects`, … — and they differ per machine and change with every project opened. It
  cannot be a pattern even in principle: `rm -rf ~/projects` is catastrophic when the project is
  `~/projects/Orchicon` and perfectly reasonable when it is `~/projects/tmp`, so the danger is a
  property of the *relationship* between the path and the scope, not of the path. That is computable;
  a glob is not.

### What Orchicon never does to the operator's machine

Every destructive operation the platform and its tooling perform is anchored to something we own, and
this is a recorded rule rather than an aspiration — each bullet below is a defect that shipped and was
found by auditing for the shape:

**The shape is: a destructive operation whose TARGET is not anchored to what the program owns, and
whose safety check is answered by the very thing being destroyed.** All four had it:

- **The installer removes only its own state.** `--force-clean` used to `rm -rf data .dev bin` — BARE
  RELATIVE NAMES, and `install.sh` contains no `cd` at all, so they resolved against whatever directory
  the operator ran the install from. From a project root that deleted *that project's* `bin/` and
  `data/`. The Windows installer had the same list (reaching `~/bin`). Both are anchored to
  `${XDG_DATA_HOME:-$HOME/.local/share}/orchicon` now, and `bin` is gone from the list entirely — the
  binary is removed by absolute path instead.
- **The post-run restore preserves the working tree before it resets one.** Restoring a project's
  shared checkout ran `git reset --hard && git clean -fd`, and the only signal it had was "the
  checkout is dirty" — which is exactly what the operator's own uncommitted work looks like. A run
  that executes in place could therefore destroy their edits. It now `git stash push -u` FIRST, so
  everything is recoverable from `git stash list`, and **if the stash cannot be made the restore does
  not run** — tidying a checkout is never worth discarding work.
- **`make clean-docker` prunes only Orchicon's containers.** It ran host-wide
  `docker container prune` and `docker volume prune`, which on a machine with any other Docker work
  removed *theirs* — and Orchicon's own data is a bind mount, so it owned none of what it deleted.
  Containers are now filtered by our `orchicon-instance` label, and the volume prune is gone: there is
  no filter that makes an anonymous volume ours, so an operator who wants that runs it themselves.
- **Tests never name a real path or device as an operand.** A test that EXECUTES a real binary through
  the real shim does not "fail an assertion" when the shim allows it — it runs the binary. That makes
  such a test FAIL-OPEN whenever its safety depends on the verdict of the very mechanism it is
  checking: correct for exactly as long as the shim agrees with the assertion, and destructive the
  moment it does not, for any reason and in any environment. The suite therefore names only targets it
  owns — `t.TempDir()` for every absolute case, `cmd.Dir` for the relative ones (`..`, `../../escape`,
  which still exercise the traversal the shim must refuse), and the never-allow operands too, since the
  class arm refuses on the BINARY NAME regardless of what the arguments say. The rule for anything
  added here: a target a real binary could damage if the guard failed is a target that does not belong
  in a test.

**The build does not depend on the operator's shell profile either** — `atlas` and `buf` each resolve
through a prefer-own-copy-then-PATH rule, so a rebuild works in a bare shell. That was the same class
of fragility from the other direction: the tool was present at `.dev/tools/bin/atlas` and unreachable
because the profile that put it on PATH had been emptied.


### Turn durability

An Ask turn is a long, expensive, non-deterministic thing, so its partial work is preserved rather
than discarded:

- **A turn that dies mid-work keeps its work.** Text *and* reasoning stream as deltas; if the turn
  ends abnormally (a stall, a reply timeout, a provider error, a dropped serve) the deltas are
  folded into the durable record instead of being lost. This is why an interrupted turn leaves both
  the answer so far and the thinking so far, rather than an error bubble over an empty row.
- **A stalled turn says what stalled** and names the model, because a rate-limited or unavailable
  provider looks exactly like a "stuck" model to the operator.
- **Stop** persists the partial reply with the stop notice, rather than throwing the content away.

### Environment overrides

| Variable | Default | Purpose |
|---|---|---|
| `ORCHICON_PERMISSION_POLICY` | *(per-instance data dir)* | The permission policy file to read |
| `ORCHICON_ASK_CONSENT_WAIT` | *unset — no bound* | Optional **leash** on a permission card. Unset (the default), a card waits for the operator **indefinitely**: answering it, stopping the turn or sending a new message is what ends the wait, because an expiry is what took a card away from an operator who stepped away from their keyboard (their rule: "the cards should just wait for the user no matter what"). Set it to a duration to restore the old fail-closed expiry — a timeout is then a **denial**, and the model is told it expired rather than that the operator refused |
| `ORCHICON_GUARD_POLICY` / `_PROJECT` / `_GRANTS` / `_ONCE` / `_FULLSEND` | *(set by the plane)* | The interactive guard shim's per-invocation contract. Set by the Ask path; not operator-set |


---

## Operator Setup (Adapters)

The multi-adapter surface (ADR-0003..0010) is configured under **Settings → Adapters**. This is the operator-setup guide for the `orchicon`-kind native adapter and its providers.

### Providers (Settings → Adapters)

Orchicon ships built-in provider profiles and lets you add custom OpenAI-compatible entries:

| Provider | Id | Kind | Token (secret) |
|---|---|---|---|
| Anthropic | `anthropic` | native Messages | `ANTHROPIC_API_KEY` |
| OpenAI | `openai` | Chat Completions | `OPENAI_API_KEY` |
| OpenRouter | `openrouter` | Chat Completions | `OPENROUTER_API_KEY` |
| OpenCode Zen | `opencode` | Chat Completions | `OPENCODE_API_KEY` |
| OpenCode Go | `opencode-go` | Chat Completions | `OPENCODE_API_KEY` |
| CommandCode | `commandcode` | dual-transport | `COMMANDCODE_API_KEY` |
| Ollama | `ollama` | compat + native metadata | none (local server) |

- **The `anthropic` row above also backs the `claude` adapter kind.** For the native `orchicon` kind it is a bearer secret (`ANTHROPIC_API_KEY`). For the **`claude` adapter kind** the credential is instead the operator's own **host claude.ai login** — `~/.claude/.credentials.json`, written by the operator's `claude` sign-in on the host, bind-mounted into the runtime container read-write and **never stored by Orchicon**; an `ANTHROPIC_API_KEY` set on the host (or supplied as a run secret) is the env fallback.

- **Model ids are LIVE only** (probe-or-nothing, ADR-0010): the Settings eyeball lists only the ids the endpoint's `/models` (or `/v1/models`) probe returned, enriched with catalog metadata (context/output/tools/pricing). A failed probe shows the **degraded/amber** state with **zero** rows and a diagnosable log line (`sourcing: probe <url> → HTTP <code>` / `unreachable`) — never a synthesized list.
- **Custom OpenAI-compatible providers** (e.g. a llama-server gateway): the base URL **must include the version root** (`…/v1`); in container mode a loopback `localhost` base is rewritten to the host gateway (plane-aware; host **and** port preserved); auth-mode is `none` or `token`.
- **Tokens / secrets**: Settings → Adapters → a provider → add the token. It is stored in the tenant **secrets store** (AES-256-GCM at rest) under the provider's canonical env name (`ANTHROPIC_API_KEY`, `CUSTOM_<REF>_API_KEY`, …). Secret-first, env-fallback; a token rotation invalidates the probe cache (the cache key folds the bearer hash).
- **Built-in overrides apply to chat at dispatch time** (built-in ⊕ tenant settings): a built-in provider's runtime profile is resolved as the built-in default merged with the tenant's stored overrides (`EffectiveProfile` in the providers service — the SAME mapping the settings views render), so a base-URL override (e.g. ollama **cloud** instead of the local `http://localhost:11434` default) takes effect for worker sessions on the next dispatch, with the registry cache invalidated on every settings mutation. The `OLLAMA_HOST` process env remains a dev/test escape hatch of LOWER precedence than tenant settings. A disabled provider row does not fail the registry (dispatch gates own enabled-ness); a provider with no stored row resolves to the pure built-in default.

### MCP servers (Settings → Adapters → MCP)

The **MCP** tab manages OWNER-SCOPED MCP server definitions (stdio / streamable-http) — a definition belongs to exactly one project or Ask conversation; env/header values are `${SECRET_NAME}` references into the same secrets store, and the curated catalog is one-click add. Resolution is the project-owned ∪ scope-owned union (see [USERGUIDE.md §11.5](USERGUIDE.md#115-mcp-servers-and-skill-files-owner-scoped)); connections are established per session at `Start`, never at control-plane boot.

### Memory

Agent-memory session storage is configured per tenant; the memory tools (`orchicon_memory_*`) persist durable notes to `<projectDir>/.orchicon/memory.db` (scope + persistence toggles under Settings → Adapters). When a store is not configured the memory tools answer with an explicit unavailable error — never a silent no-op.

### Compaction controls

Set under **Settings → Defaults → Execution budget** (per-worker `budget_overrides`): the budget gates (`tokens`, `cost_usd`, `wall_clock_seconds`, `tool_call_count`) and the `compact_max_turns`/`compact_tiers` ladder drive the native engine's context management (see [USERGUIDE.md §11.3](USERGUIDE.md#113-settings) for the exact semantics). Compaction only fires on a **LIVE context-hint source** (never guessed context): a model that reports real metadata (Ollama `/api/show` true metadata) or a probed/catalog context window; when no hint exists the picker shows a `WARN` and compaction never guesses the window.

## Development Guide

### Local Development Loop

The fastest local development cycle — stop the dev instance, rebuild the binary + image, start it again:

```bash
make container-rebuild instance=dev     # stop dev container → build bin/orchicon + image → start dev container
make container-rebuild instance=prod    # same for the prod instance
```

`container-rebuild` **always forces a fresh frontend build** (`force-fe=1`), so a rebuilt instance is guaranteed to reflect the current source — the `fe-build` stamp check that normally skips an unchanged frontend is bypassed. This avoids the trap where a stale `frontend/dist` silently ships the previous UI after a rebuild (the repeated "my frontend fix went invisible" failure): the one-command rebuild is a trustworthy "rebuild dev / rebuild prod and test recent changes" flow.

Or run the individual steps:

```bash
make build                              # bin/orchicon (frontend + container configs embedded)
scripts/container.sh down dev           # stop the dev instance
scripts/container.sh up dev             # start it again with the new image
```

For a fast Go-only iteration (no frontend change), `make build` alone uses the stamp-checked `fe-build` and skips the frontend rebuild; pass `force-fe=1` (`make build force-fe=1`) to force it.

### Dual-Instance (dev + prod containers)

Orchicon can run two isolated single-container instances side by side: a **dev** instance (`orchicon-cnt-dev`, http://localhost:8080, Grafana http://localhost:3002) for daily development and a **prod** instance (`orchicon-cnt-prod`, http://localhost:8091, Grafana http://localhost:3003) for the production pipeline. They share no ports, databases, or state — restarts to one never affect the other. `scripts/container.sh up dev|prod` manages each, reusing the compose-era Postgres volumes so data carries over. See §Single-Container Deployment.

### Single-Container Deployment

The entire Orchicon stack — Postgres, NATS, the Grafana telemetry plane (Tempo, Loki, VictoriaMetrics, OTel collector, Grafana), and the control plane — can run in **one container**. The `orchicon` binary is the PID-1 supervisor (`orchicon container`, `cmd/orchicon/container.go`):

- spawns children in dependency order (postgres → nats → telemetry → control plane), gating on readiness probes
- runs the **services only** when `ORCHICON_CONTAINER_SERVICES_ONLY=1` (the plane then runs on the HOST — §Host residency below), logging the decision explicitly
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
- **Worker executions**: the container ships the `opencode` runtime. **Mounts are scoped**:
  - **project roots** (`ORCHICON_PROJECT_ROOTS`, default `$HOME`): each root is bind-mounted at its **identical host path**, so **every `project_dir` under a root works the moment it is created** — no restart, no extra command. This is what makes "create a project for the directory I'm standing in" immediate: declare a root once, then create projects freely beneath it. Narrow it when you want a smaller blast radius: `ORCHICON_PROJECT_ROOTS="/home/me/projects:/srv/work"`, or `none` to mount no roots. Roots are mounted **before** the read-only scoped mounts below, and that order is load-bearing: where mounts nest, the more specific destination wins, so opencode's config stays `:ro` inside a writable `$HOME`.
  - `~/.config/opencode` (read-only) + `~/.local/share/opencode` (rw) so workers use your real model providers.
  - **project dirs/files from a manifest**: the control plane writes `/var/lib/orchicon/project-mounts` (every `project_dir` + `context_files` from the projects table **and every work item's `context_files`**, refreshed every 30s). `container.sh up`/`rebuild` mounts each listed path at its host location. **Only needed for paths OUTSIDE every root** — a path under a root is already visible. For one that is not, run `scripts/container.sh sync-mounts [dev|prod]` to apply; Docker can't add bind mounts to a running container, so `sync-mounts` compares the manifest to the live container's mounts and recreates it when any are missing (a path already covered by a root counts as present, so roots do not trigger needless re-creates).
  - Extra paths: `ORCHICON_PROJECT_MOUNTS` (space-separated host paths).
  - **Container DNS**: `up` pins the container's resolvers at **create** time, because Docker copies the host's `/etc/resolv.conf` only when the container is created. On a guest network (hotel, hotspot, in-flight) the host's nameserver is the access-point gateway, which usually refuses or blackholes UDP/53 until the portal is authenticated — so a container created there keeps a dead resolver forever: raw-IP egress is healthy, but every provider hostname fails to resolve, and `docker start` cannot rewrite `resolv.conf`. `up` therefore probes the host's nameservers and pins a working set: loopback/stub addresses (systemd-resolved's `127.0.0.53`) are dropped because they are dead *inside* the container even though they answer on the host; the host's remaining nameservers are kept when one of them answers (preserving corporate/VPN split-horizon DNS); public resolvers (`1.1.1.1 8.8.8.8`) are used when none does. When **neither `dig` nor `nslookup` is installed** it cannot tell a dead gateway from a healthy-but-unverifiable resolver, so it keeps the host's own resolvers — Docker's old behaviour — rather than guessing and silently breaking internal names; install `bind` (Arch/CachyOS) or `bind-tools` (Debian/Ubuntu) to let the captive-portal fallback engage automatically. Recreating is implied: `up` recreates the container when the desired resolver set differs from the running one's, exactly as it already does for changed mounts. Override the whole decision with `ORCHICON_CONTAINER_DNS="1.1.1.1 8.8.8.8"`. Portal sign-in itself stays the **host's** concern, not the container's.
  - **Ownership**: the control plane and its worker subprocesses run as **your host user** (`id -u`/`id -g` passed via `ORCHICON_HOST_UID/GID/HOME`), so files workers create in mounted project dirs are owned by you, not root. Infra processes keep their own users (postgres uid 70, telemetry root).

#### Who can see what: the trust model

Orchicon separates **where it may look** (project roots — declared once) from **what a piece of work is about** (`project_dir` — per project). Permission is granted by creating a project, and bounded by the roots you declare. The two execution paths have **deliberately different reach**:

| | Ask Orchicon (Brainstorm / Iteration / Quick Work) | Workflow executions |
|---|---|---|
| **Where it runs** | **In the control-plane container** — the in-process `opencode serve` that hosts a session per conversation (`internal/opencode/servehost.go`) | **A per-run runtime container**, created by the host-side runtime daemon |
| **What it can see** | **Everything in the declared roots.** With the default root that is your whole `$HOME`, and its file/shell tools run there | **Only the project's `project_dir` plus declared `context_files`** (`internal/runtime/lifecycle.go`). No `$HOME`, no `~/.ssh`, no git credentials |
| **Sandbox** | The OS-level execution guard applied to the session's PATH, plus opencode's per-session directory scoping | A container with its own filesystem view, and a deny-by-default `orchicon-plane` credential unless the worker carries a `role_ref` |
| **Trust it when** | You are talking to it directly: it is your agent doing what you asked, so it is given your reach | Unattended work against a repo you may not have read. A prompt-injected worker cannot reach your SSH keys — they were never mounted into its container |

**Be aware of the difference before pointing either path at a repository you do not trust.** The practical consequence: with `$HOME` as a root, the Ask agent can read anything under your home, `~/.ssh` included, while a *workflow* execution of the same project cannot reach it. To shrink the surface, narrow `ORCHICON_PROJECT_ROOTS` — that narrows **both** paths, since roots gate what the plane can see at all. Workflow containers are still confined to the project dir regardless, so narrowing roots only ever reduces the Ask agent's reach.

When a `project_dir` sits outside every root the control plane cannot see it, and orch's launch prompt says so **at the moment you create the project**, with the command to fix it — rather than letting a worker fail later to find your files.
- Published image: `ghcr.io/beardedparrott/orchicon` (built + pushed by the release workflow on every version tag, tagged `vX.Y.Z` + `latest`).

**Environment variables** (all optional):

| Variable | Default | Purpose |
|---|---|---|
| `ORCHICON_TELEMETRY` | `embedded` | `none` skips the telemetry processes (≈96 MiB); `remote` skips them and exports OTLP to your own collector |
| `ORCHICON_DATA_DIR` | `/var/lib/orchicon` | Persistent state root (postgres, nats, telemetry data, configs) |
| `ORCHICON_GRAFANA_PUBLIC_URL` | `http://localhost:8080/grafana` | Grafana's public root_url (change when publishing on other ports) |
| `ORCHICON_*` | — | Any control-plane env var (DSNs, ports) overrides the container defaults |

#### Host residency: the plane on the host, the services containerized

The control plane can run as a **host process** while the same instance's Postgres, NATS and telemetry plane keep running inside the container. Residency is a **per-instance, opt-in launch setting** read by `scripts/container.sh`, so dev can migrate first while prod stays exactly as it is:

| Setting | Where | Default | Effect |
|---|---|---|---|
| `ORCHICON_PLANE_RESIDENCY` | launcher env (`up`/`down`/`rebuild`) | `container` | `host` starts the container in **services-only mode** and the plane as a host process |
| `ORCHICON_CONTAINER_SERVICES_ONLY` | set by the launcher *into the container* | unset | `1` ⇒ `cmd/orchicon/container.go` skips the plane child |
| `ORCHICON_SERVE_STATE_DIR` | host plane env | `.dev` | per-instance PID/log root for `serve --detach`/`--stop`, so two host planes never share one PID file |

**Host is the default through both entry points that rebuild an instance, and the two differ deliberately.** `scripts/container.sh up|down|rebuild <inst>` resolves `${ORCHICON_PLANE_RESIDENCY:-host}`; `make rebuild-dev` / `make rebuild-prod` pin `residency=host` as a target-specific override; `make container-rebuild <inst>` keeps the Makefile's own `residency = container` variable. So `scripts/orchicon.sh start dev` and `make rebuild-dev` both give a host plane with no setting at all. The rollback is one word — `make rebuild-prod residency=container` puts prod's plane back inside its container — and each instance's shape is independent, so one can migrate while the other does not.

**What is published in host mode** — every service bound to **`127.0.0.1` only** (a database and an internal event bus must never be on the LAN), on ports that are disjoint per instance:

| Service | dev | prod |
|---|---|---|
| Postgres | 5432 | 5433 |
| NATS / monitoring | 4222 / 8222 | 4223 / 8223 |
| OTLP gRPC / HTTP | 4317 / 4318 | 4319 / 4320 |
| Tempo | 3200 | 3201 |
| Loki | 3100 | 3101 |
| VictoriaMetrics | 8428 | 8429 |
| Grafana | 3002 → 3000 | 3003 → 3000 |
| Plane HTTP | 8080 | 8091 |

In services-only mode `postgres` is started with `listen_addresses=0.0.0.0` (default mode keeps `localhost`), and the supervisor idempotently appends a `host all all <gateway>/32 trust` rule to `<data-dir>/postgres/pg_hba.conf` on every boot, where `<gateway>` is the container's default-route gateway: the connection arrives through the published port, so postgres sees that gateway as its peer, never loopback. The rule names that single address on purpose — every other container on the same bridge shares it, and a wildcard rule would hand them password-less superuser access to this instance's database. Only when no gateway can be detected does it fall back to the wide `0.0.0.0/0` + `::/0` rules. Exposure stays bounded by the loopback-only publish, which is the security boundary.

**The host plane's profile** is printable with `scripts/container.sh shape <inst>`, and every value comes from the instance table (never a shell profile or a shared env file):

- `ORCHICON_HTTP_ADDR=:<PLANE_HTTP_PORT>` and `ORCHICON_HTTP_EXTRA_BIND=<docker-bridge-ip>:<PLANE_HTTP_PORT>` — the plane binds **both**: its loopback address (host clients: `orch`, the GUI) and the docker bridge at **this instance's** port, so the run containers on that bridge can dial it. The bridge address is resolved from the host (docker's own IPAM config, else `docker0`; pin it with `ORCHICON_DOCKER_BRIDGE_IP`), never hardcoded to `172.17.0.1` and never a wildcard — the plane must not be reachable from another machine. `scripts/container.sh plane-bind <inst>` prints the same two values;
- `ORCHICON_PLANE_PUBLIC_URL=http://<docker-bridge-ip>:<PLANE_HTTP_PORT>` — DERIVED per instance from that bind (`bridge_bind_env` is the one place either value is computed) and it is what the plane hands each run container it creates (`ORCHICON_PLANE_URL`), so an instance can only ever hand out its own address: dev and prod listen on different ports, and a shared `8080` literal would make one instance's workers dial the other's plane. Setting `ORCHICON_PLANE_PUBLIC_URL` per instance overrides the derivation; **never** export it from a shared shell profile. Every runtime container is created with `--add-host=host.docker.internal:host-gateway`, so a manually started host plane (no bind at all) is still reachable by that name;
- `ORCHICON_POSTGRES_DSN` / `ORCHICON_NATS_URL` / `ORCHICON_OTEL_ENDPOINT` (+ Tempo/Loki/VictoriaMetrics/Grafana URLs) point at the published loopback ports;
- `ORCHICON_DATA_DIR=$HOME/.local/share/orchicon-<inst>` and `ORCHICON_BLOB_DIR=<data-dir>/blobs` — the KEK (`<data-dir>/secrets/kek`) and ask-history move with it. The **first** switch-over copies the existing container volume (including `secrets/kek`) into the host dir, so existing tenant secrets keep decrypting; an existing host KEK is never overwritten;
- `ORCHICON_RUNTIME_SOCKET` points at the host runtime daemon's real socket;
- `ORCHICON_CONTAINER_MODE` is explicitly **unset** for the host plane — a stray export must not flip it into container semantics (custom providers stored as `http://localhost:…` must keep that URL on a host plane, §Providers).

`orchicon container` logs the services-only decision plainly at boot (`services-only mode: postgres, nats and telemetry run in this container; the control plane runs on the HOST`), because a container silently running without a plane looks like a failed boot. The image's plane `/healthz` healthcheck is replaced with a services probe in that mode.

**Steady-state commands:**

```bash
scripts/container.sh shape dev        # what the launcher WOULD do (no side effects)
scripts/container.sh verify dev       # what the instance ACTUALLY runs (ports, env, supervisor log)
scripts/container.sh plane-stop dev   # stop the HOST plane; the services container keeps running
scripts/container.sh plane-start dev  # start it again — no container restart
```

**Migration rule:** dev and prod migrate independently and coexist in different shapes; a rebuild of one instance must never alter the other's. `scripts/container.sh verify <inst>` is the check.

**Measured footprint** (single container, this stack): full telemetry ≈ **384 MiB** resident; `ORCHICON_TELEMETRY=none` ≈ **96 MiB** — vs ~2.7 GB for the ClickHouse-era compose stack.

Dual-instance (dev + prod) is two containers with offset published ports (`-p 8080:8080 -p 3002:3000` and `-p 8091:8080 -p 3003:3000`), separate data volumes. An instance may instead be **host-resident** (its container runs the services only, its plane runs on the host) — see §Host residency above; the two instances may be in different shapes at the same time.

### Workflow Runtime Containers

Worker executions run inside **one short-lived container per active workflow run** (Azure Pipelines self-hosted agent model). It is created when a run leaves `pending`, every execution for that workflow is dispatched into it, and it is killed when the run reaches a terminal state (`completed` / `failed` / `aborted`). Everything inside is ephemeral — installed tools, caches, and sessions are wiped on teardown, so each workflow starts from a pristine, fully-armed environment.

> **Platform note (Windows):** the entire runtime layer — the daemon, its POSIX unix socket, and the container mounts — is Linux/POSIX-only and is **not ported to native Windows**. On Windows the stack runs inside **WSL2** (see [USERGUIDE.md §1 — Installation](USERGUIDE.md#1-installation)): the WSL2 kernel is real Linux, so the runtime containers, the daemon, and the single-container stack work exactly as on Linux. The Windows installer (`scripts/install.ps1`) only provisions WSL2 and installs the Linux binary into the distro.
>
> **Orchicon MCP availability:** the built-in Orchicon MCP server is registered by default for **in-process** executions and Ask Orchicon chat. Inside runtime containers the **sandbox-scoped** Orchicon MCP is **NOT** registered on base/`:gui` images — the sandbox has no route to the plane's Postgres and is deliberately kept DB-credential-free (§MCP & the Orchicon MCP Server). On **`:orchicon-dev`** images the container's serve registers it against the **in-sandbox plane's** Postgres instead (§Sandbox plane below) — workers get `orchicon_*` tools against their own disposable DB, never the host plane's. Separately, the **plane-channel** Orchicon MCP (`orchicon-plane`, the `orchicon_plane_*` tools) is registered on **every** runtime image — base, `:gui`, web-research, `:orchicon-dev` — whenever the run's worker role grants it: plane access is role-gated, never image-gated (§MCP & the Orchicon MCP Server). Idea spawning on the plane channel is **explicit and dedicated**: `orchicon_plane_list_idea_items` reads the Idea Cloud (state="active" for pending triage — the dedupe gate's primary read — and state="rejected" for previously dismissed spawns, the rejection memory that must be checked first so a human's rejection is never re-proposed) and `orchicon_plane_create_idea_item` forces an IDEA landing server-side from the run's trusted context (loud refusal on a missing/non-idea provenance block, and the create envelope self-verifies with `landed_status`/`idea_state`), so a spawn can never silently land as a plain pending item.

**Components:**

- **`orchicon runtime-daemon`** (host process): the only process with access to the Docker socket. Serves a narrow HTTP API over a unix socket (default `/tmp/orchicon-runtime/runtime.sock`, bind-mounted as a **directory** into the supervisor container at `/var/run/orchicon-runtime`): lease/release warm runtime containers (`POST`/`DELETE /v1/runtimes`), build/remove runtime images (`/v1/images`). Every request is validated — image allowlist (the base + `ORCHICON_RUNTIME_IMAGES` stock images + any locally-present image carrying the inherited `org.orchicon.runtime-base` label), mount sources restricted to the projects root — so the control plane can never create an arbitrary container. The daemon owns the **warm pool** (§below): leases are daemon-resident, the pool is reset wholesale at daemon start (which also reaps plane-down leaks), and clean containers are idle-reaped. Started by `scripts/container.sh up`; manage with `scripts/container.sh runtime-daemon` / `runtime-stop`. **Freshness-gated**: `container.sh up` (and therefore every `make container-rebuild` / `make rebuild-dev` / `rebuild-prod`) byte-compares the daemon's stable binary copy (the file bind-mounted into every runtime container) against `bin/orchicon` and restarts a stale daemon — a rebuild thus picks up new runtime-side code (MCP toolset, supervisor) without a manual daemon restart; the restart also reaps every pooled container from the pre-rebuild binary.
- **`orchicon runtime-supervisor`** (PID 1 inside each runtime container): listens on a unix socket (`/tmp/orchicon-agent.sock`) and answers the daemon's two requests — a `ping` (container readiness) and the `serve` handshake. Hosts the container's **opencode serve** (detached child, `Cmd:"serve"`, 0.0.0.0:4096, guard + **stable** XDG data dir `/tmp/orchicon-serve-data` so sessions survive restarts), answers idempotent serve handshakes (owning the serve password, liveness-gated — a wedged serve is restarted, not reported as up), and runs a **serve watchdog** that polls `/global/health` and restarts the serve in place (same port + password) when it stops answering. On images that bake the pieces (currently `:orchicon-dev`) it also boots the **sandbox plane** (§Sandbox plane below) in the background. Builds the execution-guard shim in-container so workers run under the same `rm`/`sudo`/`dd`/`mkfs` path-scoped safety guard as the in-process path. (The one-shot exec/stream/reconnect machinery was removed with the transport it served.)
- **`orchicon runtime-client`** (in-container): forwards a request from the daemon (via `docker exec`) to the supervisor socket and relays the answer back, so the daemon never needs shell-level access to the container.

**The orchicon binary is mounted, never baked:** the runtime images contain **no `orchicon` binary** — the daemon bind-mounts **its own executable** read-only at `/usr/local/bin/orchicon` in every runtime container it creates, so the container can exec `orchicon runtime-supervisor` / `runtime-client` without the binary being baked into the image (the same "mount, never bake" pattern as the adapter CLIs). The daemon CLI **self-copies its binary to a stable path next to the socket at startup** (`cmd/orchicon/runtime.go` `copySelf`), so dev hygiene that deletes the original (`make clean` removes `bin/orchicon`) can never orphan the mount — the copy is what gets mounted, refreshed only when the daemon is rebuilt and restarted. The mount is a **hard dependency**: the entrypoint is `orchicon runtime-supervisor`, so if the executable is unavailable the daemon **fails the container create with a clear error** rather than creating a container that would exec a missing binary and die (never a silent skip).

**Lifecycle (warm pool + serve gate):** the `WorkflowReconciler` **leases** a warm runtime container when a run leaves `pending` (mounting the project's `project_dir` plus any project/work-item `context_files` paths that lie outside it) and **releases** it when the run reaches terminal — the release resets the container in the background (§Warm Pool below). **The run-start serve gate** (`runtime_ready`, §Persistent Worker Sessions) guarantees no execution is dispatched until the container's opencode serve is **proven usable** (L1: `/global/health` + a real session-create round-trip) — and is **adapter-aware**: a run whose worker steps all resolve (via their model_refs, ADR-0003 single source of truth) to serve-less kinds (native `orchicon`) arms with `runtime_ready=true` and the `no-serve` sentinel image, creating **no** container at all (the in-process native engine needs no serve); mixed runs (≥1 opencode step) keep the full gate. A 30s adopt pass at boot ensures a lease exists for every active run (the daemon's pool reset at start covers orphaned containers from a plane-down/daemon-restart). **Instance-scoped**: every runtime container is labeled with its owning instance (`orchicon.instance=dev\|prod`), so dev and prod sharing one daemon never reap each other's runtimes. The same boot pass runs an **execution-liveness reaper**: executions still `running` whose session runner is gone (plane restart, lost runtime container) are failed with `execution lost: control plane restarted or runtime container gone` and their work item transitions to failed, so the workflow's recovery step re-dispatches in a fresh runtime instead of the run getting stuck. The adapter **self-heals** on dispatch too: it re-leases the run's container before every execution (the daemon's checkout is idempotent per run), so a recovery re-dispatch can't race ahead of the adopt pass. Headless `orchicon serve` (no daemon socket) disables runtime containers, stays in-process, and still reaps in-process executions orphaned by a restart.

**Warm pool** (`internal/runtime/pool.go`): runtime containers are keyed by **environment** (image + project mounts + a fingerprint of the read-once **host inputs** baked into every container at create time — opencode config/auth, the adapter install, and the resolved GH token) and reused across runs of the same project instead of cold-started per run — the container creation + serve bring-up that used to happen on the dispatch hot path now happens once, at lease time, off the hot path. The host-input fingerprint makes ANY change to a read-once host input force a fresh container on the next checkout (a warm container silently caching the stale value is never reused), while unchanged inputs keep the warm-reuse path — no perf regression. Leases are **exclusive per run** (a container is handed to exactly one run at a time). On release, the container is **reset in the background** — `docker rm -f` + recreate with the identical spec + warm the serve — so the pool only ever hands out **pristine** environments: nothing from the previous run's state (installed packages, `/tmp`, opencode sessions/data) crosses the boundary, preserving the security property that motivated the sandbox. Dispatch never blocks on a reset — a checkout that finds the pool empty just creates fresh (the cold path, which the run-start gate absorbs). Clean containers are idle-reaped (`ORCHICON_RUNTIME_POOL_IDLE`, default 10m) and capped per environment (`ORCHICON_RUNTIME_POOL_CAP`, default 1). Two concurrent runs on the same environment each get their own container (on-demand create) — never shared. Leases are **self-healing**: every idempotent re-checkout of an active run (the run-start gate, the adapter's dispatch self-heal, and the plane's 30s adopt sweep) renews the lease; `reapStaleLeases` removes a leased container whose lease has stayed idle past `ORCHICON_RUNTIME_LEASE_MAX` (default 30m — 60× the renewal cadence, so a wedged host can never lose a live run, while an abandoned lease — the aborted-run class whose terminal release was lost — dies within half an hour) and clears dangling lease mappings. Separately, `hostInputsFingerprint` folds the daemon's own binary into the pool key, so a rebuilt binary invalidates every warm container.

**Stuck-run detection (no leaked containers):** a container is only reaped when its run reaches a terminal state, so any run that can never progress would hold one forever (the adopt sweep treats every running run as active and keeps the container alive). The reconciler therefore fails a run **at start** — reaping the container and, for a sequence child, halting the parent's chain — when the published version has an **empty step DAG** (`steps=[]`), when its workflow **version row is gone** (workflow deleted / raw-seeded run), or when the runtime image can't be resolved (this failure is now committed, not rolled back by the deferred rollback). It also un-wedges **orphaned step references**: `pollTaskStep` fails a running task step terminal when its work item was hard-deleted mid-run, and falls through to the recovery block (after the dispatch-link grace) when its execution row is gone — instead of waiting forever, which previously left the run `running` and its container up indefinitely.

**Runtime adapter CLIs are mounted, never baked:** the images contain **no adapter binary**. When a run's boot profile demands OpenCode — decided **per run** from the model refs that run will dispatch — the daemon mounts the operator's host `~/.opencode` install (read-only) into that runtime container and puts its `bin/` on PATH, so the supervisor can exec `opencode`. A run on the built-in engine mounts nothing at all, and `container.sh`/`orchicon install` add the same mount to the main container, conditionally, for in-process dispatch. The supervisor's `argv[0]` allowlist (`runtimeBinAllowlist` in `internal/runtime/agent.go`) lists the adapter binaries Orchicon may exec — `opencode` today; the `claude` adapter's bridge adds its entry when it execs the CLI through the supervisor, and `codex` gets one when that adapter lands. This is the licensing-safe pattern for all future adapters: **the product mounts the operator's own install; it never ships, downloads, or redistributes the CLI.**

**Claude adapter — host-account auth provisioning:** the Claude adapter (adapter kind `claude`, provider `anthropic`) authenticates with the **operator's own host account**. The default is *bring your own authenticated host*: the operator signs in on the host with the `claude` CLI (first-run browser login, `/login`, or `claude auth login`), or sets `ANTHROPIC_API_KEY` in the host environment. Orchicon **never** presents a claude.ai login, never stores, bakes, or redistributes an Anthropic credential, and never caps or resells Anthropic rate limits — **no Orchicon UI or API is an Anthropic auth surface**. When a run's boot profile demands `claude`, the daemon bind-mounts the operator's host `~/.claude` and `~/.claude.json` **read-write** — the `claude` config home is the **only read-write adapter install**: a session writes its transcript tree under `~/.claude/projects/` and the CLI rewrites `~/.claude.json`, while every other adapter install stays read-only. Alongside them it mounts, read-only, the CLI launcher `~/.local/bin/claude` and its install root `~/.local/share/claude`. The launcher is a **symlink** into the install root (the native-installer layout — the structural analogue of opencode's `~/.local/share/opencode`); the daemon mounts at identical absolute host paths, so **both** must be declared or the link dangles inside the container. `~/.local/bin` is prepended to the container PATH *and* to the supervisor's child-process PATH. Sessions launch **non-bare** so the persisted `~/.claude/.credentials.json` login (mode 0600) is honored — bare mode reads no OAuth/keychain, so it is deliberately never used for this adapter. The adapter CLI install roots also feed the warm-pool host-input fingerprint, so a CLI upgrade or reinstall invalidates a warm pooled container instead of serving a stale CLI. If the host has **no** Claude credential at all, a `claude`-demanding container create fails fast with *"Claude is not authenticated on the host — run `claude` and sign in on the host, or set ANTHROPIC_API_KEY on the host, then re-run"* — never a silent hang and never a misleading generic error (`adapter.ClaudeAuthFailure` classifies an execution-time auth failure to this same canonical text; `adapter.ClaudeAuthRequiredMessage` is its single source). The adapter is named **Claude** (provider `anthropic`), not "Claude Code", per Anthropic's branding guidance for products embedding it.

**Security model — no root process in the runtime container:**

- The runtime container runs as the **host user's uid** (`ORCHICON_HOST_UID`, default 1000) with the image rootfs **chowned to that uid**, so workers have full write control over the ephemeral filesystem (they can install tools) while any bind-mounted project directory is written as the host user — never as root. A worker cannot `chown` a project file to root or escalate to the host.
- `dpkg` refuses to run as non-root, so system packages (python, node, build-essential, gh, …) are **baked at build time** (`deploy/runtime/Dockerfile`); runtime installs use user-space package managers (`pip` with `PIP_BREAK_SYSTEM_PACKAGES`, `npm`, `mise`, `uv`, `curl`) into the chowned rootfs / ephemeral `$HOME`.
- The daemon mounts `~/.config/opencode` and `~/.local/share/opencode` **read-only**; the supervisor redirects each worker's opencode state to an ephemeral `XDG_DATA_HOME` under `/tmp` (seeded with `auth.json`), so sessions/keys never touch the host's real opencode data. Git identity + credential store are mounted read-only (PR/merge workers need them). The `claude` adapter's config home (`~/.claude` + `~/.claude.json`) is the **one read-write** adapter mount — a claude session must write its transcript tree — while that adapter's CLI launcher and install root stay read-only.
- Per-runtime resource limits: 4 CPU / 4 GB memory / 2 GB tmpfs `/tmp` (configurable via `ORCHICON_RUNTIME_CPUS` / `_MEMORY` / `_TMPFS` on the daemon).

**Runtime images (self-service builds):** the image a workflow run's container uses is chosen **per work item** (`work_items.runtime_image`, backend-stamped to the base image when empty). The **Runtime Images** page (sidebar) lets you define and **build** custom images on the host runtime daemon: a structured form (apt packages, toolchain lines, env) with a **live Dockerfile preview** that doubles as an advanced raw-Dockerfile editor, plus a **Deploy** button that streams the `docker build` log. Editing a ready image reverts it to draft so it must be rebuilt; delete removes the spec row (custom images also remove the local Docker image — gated on no active run using it; canned rows and the daemon's base image are never `docker rmi`'d). Every build is guaranteed to derive from the base image — the daemon rewrites the Dockerfile's `FROM` line to the base and injects the `org.orchicon.runtime-base=true` label (plus `org.orchicon.runtime.spec-version=<n>`, the spec version the image was built from), which is also the container-create gate (a locally-present image carrying that inherited label is accepted without a separate registration). **Deploy is idempotent**: the image's `version` is the "spec changed" signal (spec edits bump it; build-flow status transitions do not), and the row records `built_version` — the version the current `ready` image was actually built from. Re-deploying an unchanged ready spec short-circuits (`built_version == version` → instant "up to date", no `docker build`, no prune); editing the spec bumps `version`, so the next Deploy rebuilds. The workflow-run start resolves the image (template → bound work item; one-shot → the WORK_ITEM markers' items, all must agree or the run fails at start) and stores it on the run, so a self-healed container is recreated with the identical image.

**Canned (stock) runtime images:** the shipped images — the **base**, **`:gui`** (`deploy/runtime/Dockerfile.gui`, headless Qt/tkinter/X11 libs) and **`:dev`** (`deploy/runtime/Dockerfile.dev`, the dev image — Go/Node/buf/atlas plus a baked PostgreSQL 15 for in-sandbox DB testing) — are **seeded as normal, editable `runtime_images` rows (`source='stock'`) on every boot**, exactly like canned workers. They appear in the main Runtime Images list (with a **stock** badge + spec version/built_version), are editable in the advanced Dockerfile editor, deployable, and deletable (a deleted canned row is re-seeded next boot). The seeder writes the shipped Dockerfile template plus a versioned seed marker (`# orchicon.seed=<sha12>`); a row that is still an intact seed is reconciled in place, a stale seed (template changed) is rolled forward — `version` bumps, `built_version` lags, and the row is **auto-built** asynchronously, which prunes the previous version of the tag — while any row whose body no longer matches its marker is a user edit and is **never touched**. When the local docker image is already current (container.sh / the installer built it before the plane booted) the seeder records `built_version = version` instead of rebuilding. The daemon's `GET /v1/images` now reports each image's `org.orchicon.runtime.version` / `spec-version` labels so the seeder can reconcile. Instances are independent: dev and prod each seed their own canned rows in their own DB.

**Building & testing Orchicon itself (the `:dev` image):** to have a worker build and test the Orchicon repo itself, set the project's `project_dir` to your Orchicon checkout on the host and give its work items the `:dev` runtime image. The per-workflow runtime container mounts `project_dir` automatically at container-create time (`Lifecycle.EnsureForRun` reads it from the projects table — no `sync-mounts` needed for runtime containers; that script only applies to the long-lived single-container instance, where Docker can't add bind mounts to a running container). The checkout must be under the daemon's `AllowedRoots` (default `$HOME/projects`). Inside the sandbox the supervisor has **already booted the full Orchicon control plane** (§Sandbox plane below) — Postgres, NATS and `orchicon serve` — so a worker can `curl http://localhost:8080/healthz`, run `go build`/`go vet`/`make gen`/`make fe-build`, the full `make ci` DB path against `localhost:5432`, and use the `orchicon_*` MCP tools against the sandbox DB, with no ad-hoc booting. The whole plane dies with the container and never touches the plane's Postgres, preserving the no-DB-route sandbox invariant.

**Sandbox plane (full Orchicon environment in-container):** the `:orchicon-dev` runtime image bakes PostgreSQL 15 + `nats-server` (v2.10, matching the single-container image's line), and the runtime supervisor boots a **disposable, self-contained Orchicon control plane** at container start — `initdb` (once, into the stable data dir `/var/lib/orchicon-sandbox/postgres`) → `pg_ctl -w start` (trust auth, `listen_addresses=localhost`, socket dir under the sandbox data dir) → `createdb orchicon` → `nats-server -js` → `orchicon serve` with sandbox env (`ORCHICON_POSTGRES_DSN=postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable`, `ORCHICON_NATS_URL=nats://localhost:4222`, `ORCHICON_HTTP_ADDR=:8080`, `ORCHICON_GRPC_ADDR=:9090`, `ORCHICON_TELEMETRY=none` — no Grafana stack, `ORCHICON_OPCODE_SESSION_TRANSPORT=0` — the plane is an API/DB/MCP surface, never a second execution plane, `ORCHICON_SANDBOX_PLANE=1` — suppress the serve's kill-orphans pgrep so the supervisor's live serve is never SIGTERMed, `ORCHICON_INSTANCE=sandbox`, `ORCHICON_BLOB_DIR` + `ORCHICON_INDEX_CHECK_INTERVAL=0` under the sandbox data dir). The plane runs migrations + tenant/worker seeding on boot against the sandbox DB.

  **Self-gating:** the supervisor probes for a Postgres server bin dir (`/usr/lib/postgresql/<ver>/bin` containing `initdb`/`pg_ctl`/`pg_isready`/`postgres` — ALL four, a partial dir is rejected) plus `nats-server` on PATH, cached once per container lifetime. Base/`:gui` images skip the plane entirely — behavior identical to today. A **watchdog** (`watchSandboxPlane`, 15s interval) re-boots the stack via the idempotent `bootSandboxPlane` when `/healthz` stops answering; Postgres is restarted in place (pg_ctl reuses the stable cluster — no re-initdb), and a best-effort `pg_ctl stop` runs on supervisor exit (container teardown is the real guarantee). The **serve handshake** reports `plane_enabled`, the daemon publishes `CreateResponse.PlaneURL = http://<container-ip>:8080` on dev images (empty otherwise), and the **run-start gate** probes the plane's `/healthz` on the bridge IP alongside the serve L1 check — a half-initialized plane can't pass.

  **MCP against the sandbox:** on dev images the container serve's config registers the built-in Orchicon MCP (`orchicon mcp`) pointed at the sandbox Postgres — the entry's environment carries `ORCHICON_MCP_TENANT_ID` + `ORCHICON_POSTGRES_DSN` (the sandbox DSN), and the command is forced to `/usr/local/bin/orchicon` (the daemon's read-only bind-mount, guaranteed present; the plane's own executable path is not). Workers get the `orchicon_*` tools natively against their own disposable DB — never the host plane's. Create-to-ready is ~6-8s on first dispatch per run (initdb ~0.4s + serve boot + migrations), overlapping the existing ~4s opencode-serve warm; warm-pool checkouts that find the plane mid-boot simply wait in the run-start gate.

  **No-DB-route invariant preserved:** the sandbox plane's DSN is container-local trust auth — it never learns the host plane's DSN, and the MCP sidecar is pointed at the sandbox DSN. The plane dies with the container (pool reset recreates pristine).

**Build:** `make container-build` also builds `orchicon-runtime:local` (plus `orchicon-runtime:local-gui` and `orchicon-runtime:orchicon-dev`) — **version-gated, not unconditional**. Each stock runtime image is tagged with an `org.orchicon.runtime.version` label derived from the app version + the SHA-256 of its Dockerfile (`<version>-<sha12>`; the `:gui`/`:dev` versions embed the base's version so a base change cascades to them). On every `container-build` the script inspects the existing image's label and **skips the build when it matches** — the base image no longer bakes the orchicon binary (the daemon mounts its own at container-create time), so its content is a pure function of the Dockerfiles and a normal dev rebuild (binary changed, Dockerfiles unchanged) skips all three. The script also skips any image carrying `org.orchicon.runtime.spec-version` (a daemon-built image from an edited+redeployed canned row) — container.sh never clobbers a tenant-owned build. Touch a Dockerfile (or bump the app version) and the affected variants rebuild. Escape hatch: `FORCE_RUNTIME=1 make container-build` (or `ORCHICON_FORCE_RUNTIME_REBUILD=1 scripts/container.sh build`) rebuilds regardless. The release workflow ships the runtime image to GHCR (`ghcr.io/beardedparrott/orchicon-runtime:<version>` + `:latest`, plus the `:gui` and `:dev` variants) — the one-command install pulls it, and the runtime daemon defaults to that image (`ORCHICON_RUNTIME_IMAGE` overrides; local dev pins the locally-built tag). `ORCHICON_RUNTIME_IMAGES` adds extra allowlisted stock images (base always included). **Model note:** executions dispatch with the worker's pinned `model_ref`; verification workers should pin a free model (e.g. `opencode/deepseek-v4-flash-free`).

### Native (orchicon-kind) session engine

The native adapter runs worker executions in-process against the tenant's configured providers (built-in profiles ⊕ overrides, §Providers above — no opencode serve). Its turn loop (`internal/orchicon/loop.go`) settles a session ONLY on an honest terminal, with opencode-adapter parity for the completion contract:

- **Decision-signal gate** (`internal/orchicon/completion.go`): a turn that ends `stop` is settled as success ONLY when the session output carries a REAL `ORCHICON WORKER SUMMARY:` sign-off — the same marker contract, placeholder detection (`realDecisionMarkerIn`/`placeholderMarkerBody`, kept in sync with `internal/opencode/session_run.go` and the scheduler's reconciler), and completion probe (`runCompletionProbe`, budget 2) as the opencode session transport. A markerless settle-point first interjects the probe turn (asking the worker to deliver its sign-off without restarting work); a reply carrying a real marker settles normally; a session that still cannot deliver after the budget fails honestly with `stalled:missing_decision_signal:completion_probe_no_response` — the workflow's loop-decision/re-ask/fail path owns the missing signal, never a phantom `succeeded`.
- **No synthesized stop**: every provider client (`openaicompat`, `ollama` native, `anthropic`, `legacycc`) maps a stream that ends WITHOUT the provider's end-of-response signal (`finish_reason` absent, `done_reason` "", `message_stop` with no `message_delta` stop_reason, no finish event) to `StopOther` — never to a synthesized `stop`. `StopLength` and `StopOther` are FAILURES in the loop (`model terminated with stop reason …`): a length-capped turn (the hardcoded `MaxTokens: 4096`) cut the model mid-generation — the exact shape of the reported hollow successes — and a stream with no stop signal never actually ended the turn. Only a real `stop`/`end_turn`/`done_reason:stop` settles, and only through the marker gate.

### Persistent Worker Sessions (session transport)

Worker executions run through a **persistent opencode session** — the ONLY execution transport (the legacy one-shot `opencode run` subprocess path was removed: a run that cannot get a session fails fast, `failed_to_start` → workflow recovery, instead of silently degrading to a second, inferior transport). One `opencode serve` instance owns each execution's session and its agent loop; the goal is the first user message, and liveness nudges + mid-run human messages join the session's per-session prompt queue (serialized by the server, so an injected message lands at the next turn boundary).

**Serve topology** (two populations, one `SessionClient` — `internal/opencode/session.go`):
- **Always-on host serve** (`internal/opencode/servehost.go`) for the in-process population — standalone dispatches, follow-ups, Ask Orchicon later, and any execution not bound to a workflow run. Spawned by the control plane at boot, supervised by a health watchdog with restart + backoff, against a **dedicated** persistent data dir (`~/.local/share/orchicon/opencode`, seeded with the operator's model auth — never shares an `opencode.db` with the operator's own opencode). Sessions survive serve restarts. The operator's MCP servers are merged in, plus the built-in Orchicon MCP (tenant-scoped).
- **Per-workflow-run container serve** for workflow executions. The supervisor starts `opencode serve` as a detached child (0.0.0.0, fixed port 4096, same guard + **stable** XDG data dir `/tmp/orchicon-serve-data`), owns the serve password (idempotent handshake), and the daemon returns the plane-reachable base URL (`http://<container-ip>:4096`) in `CreateResponse.ServeURL`. The serve is **warmed at container-create time** (the run-start gate passes `ServeConfig` to the daemon's checkout), so dispatch never cold-starts it. The plane reaches the serve **directly on the docker bridge** — no published port, no docker-proxy (a containerized plane cannot reach the host loopback, and published-port forwarding to a serve that starts lazily races). The serve config omits the operator's MCP servers (`SkipUserMCP`): a serve eagerly connects to every configured MCP server at startup, and the `orchicon`/`orchicon-dev` entries (which `docker exec` into containers) would hang it.
 - **Serve watchdog** (`internal/runtime/agent.go` `watchServe`): the supervisor polls the container serve every 10s and restarts it (in place, same port + password + stable XDG data dir, so sessions survive and the SSE client re-attaches by id) when it stops answering `/global/health` — covering a serve that WEDGES (alive but hung), which `watchExec` (process-exit only) never sees. The `runServe` idempotent handshake is liveness-gated: a registered-but-unhealthy serve is reported as DOWN (and restarted) rather than "up", so a dispatch never burns its 30s readiness probe against a dead serve.
 - **Run-start serve gate** (`runtime_ready`, `internal/runtime/lifecycle.go` `EnsureServing`): a workflow run does NOT dispatch **any** execution until its runtime container's opencode serve is **proven usable** (L1: `/global/health` AND a real session-create round-trip). The pending→running transition persists `runtime_ready=false`; an async ensure-serving pass (one goroutine per run, idempotent, re-triggered after a plane restart) leases the container, warms + probes the serve within `ORCHICON_RUNTIME_SERVE_READY_TIMEOUT` (default 120s), and flips the flag; the `WorkflowReconciler` holds step-DAG progression (and the `TaskReconciler` belt-and-suspenders-skips dispatch) while it is false. On `:orchicon-dev` images the gate ALSO requires the **sandbox plane's** `/healthz` to answer on the container bridge IP (inside the same window) — a half-initialized plane can't pass, so no execution dispatches against a run whose sandbox environment isn't ready. This converts the old dispatch-time race — a cold-starting serve failing the first execution's 30s window, then recovery looping — into a deterministic check at run start. A serve that cannot come up **fails the run at start** with a clear error (+ container release) instead of a step-level recovery loop. Headless serve sets the flag true immediately. **Adapter-aware (ADR-0003):** the gate only applies to runs with actual serve demand — `runNeedsServe` resolves each worker/approval step's model_ref (step-pinned version → latest published, unresolvable ⇒ conservative opencode demand) and a run with no serve-dependent kind (`opencode` today; native `orchicon` never needs it) arms `runtime_ready=true` with the `runtime.NoServeImage` sentinel, never calls `EnsureServing`, and creates no container (all container paths — `EnsureForRun`, `EnsureServing`, boot `Adopt` — no-op on the sentinel). The previously-observed failure mode (a native-only run failing at start with "runtime opencode serve failed to become usable" and sitting "waiting for dispatch…" until the serve deadline expired) is structurally impossible.
 - **Model-layer wedge recycling** (adapter, `internal/opencode/session_run.go`): the serve health watchdog cannot see a serve whose `/global/health` answers but whose **model turns fail instantly** (provider/API-level `session.error`). When `ORCHICON_SESSION_ERROR_RECYCLE_THRESHOLD` (default `3`) consecutive session errors accumulate across executions, the adapter **recycles the affected workflow's runtime container** (`Kill` → the lease is released + the container reset in the background; the next dispatch's checkout builds a fresh serve) — exactly the manual fix that un-wedged a field incident where every auto-retry re-hammered a poisoned serve in ~80ms. Any non-error progress (step/tool/message) resets the counter, so a single transient failure never recycles; the env override is documented in Settings → Defaults.
 - **Session-backend infra repair at dispatch** (adapter, `internal/opencode/adapter.go`): the session-setup loop distinguishes **infrastructure** failures from worker failures. When session creation fails because the backend itself is broken — the serve **died** (dial `connection refused`) or is **alive but poisoned** (`POST /session` → HTTP 5xx, e.g. a session store that errored with `Failed to execute statement`) — the dispatcher does NOT simply burn the step's retry budget into the same hole: it **recycles the run's runtime container** (bounded by `ORCHICON_SESSION_REPAIR_ATTEMPTS`, default `3`) and re-dispatches against the freshly-built serve. Recycle (not in-place restart) is deliberate: a poisoned session store lives on disk in the container's stable XDG data dir, so restarting the serve process in place (the watchdog) reuses the poison — killing the container discards it, and the step's on-disk worktree state survives, so the work **continues** rather than restarting cold. The repair is **not first-resort**: a single infra failure is retried on the same container (a fresh session create), so a healthy parallel step on the same run container is never torn down by one blip — only a **persistent** infra failure (consecutive count ≥ `ORCHICON_SESSION_INFRA_THRESHOLD`, default 2) triggers the recycle. Each repair re-runs the container-create serve handshake as its health gate before the next dispatch; the cycle is bounded by `ORCHICON_SESSION_REPAIR_ATTEMPTS` (default 3). Only after the repair budget is spent does the dispatch fail (`failed_to_start` → the step-level retry/recovery loop, which is bounded separately). This is the change that turns "session died → workflow failed" into "session died → container rebuilt → workflow continues", and it is what the 2026-08-22/23 SDLC incidents (run `01M0NSHWJYWSDHGP12676YAN8X` — refused; run `01M0P08V7D8AY7KT8AEJ924DKR` — `POST /session` 500s after a `Failed to execute statement`) exercise.
 - **Recovering-step never-limbo** (reconciler, `internal/scheduler/workflow_reconciler.go`): a recovering step whose dispatch gate can never resolve (its recovery row never materializes, or its terminal `resumed` recovery's seed never becomes resolvable — neither an active recovery nor a pending/running state, so nothing will move it) previously held the run "running" indefinitely (observed 45+ minutes). `recoveringStallTimeout` (`ORCHICON_RECOVERING_STALL_TIMEOUT`, default 15m) now **fails the step terminal** once the run has been running past the cap while the step stays un-resumable for one of those two dead-end reasons — letting the run engine set the run FAILED so the **Retry failed run** action surfaces (the step's attempt budget and in-flight recovery holds are untouched; a value `< 1s` disables the cap).
 - **Fail-fast dispatch**: `sessionClientFor` returning nil (no host serve, or the daemon's checkout failing to bring the container serve up) is a hard error → `failed_to_start` → workflow recovery. The daemon propagates serve-start instead of silently degrading; with the run-start gate, a container with a dead serve is recycled by the session-backend repair above rather than failing the whole run on first contact.
 - **Context efficiency** (lever that the ~99M-token/6-run incident exercises): (1) `external_directory` additionally allows the Orchicon-own `.orchicon/**` subtree so step workers in isolated worktrees can read the run's `.orchicon/<run>/` summary/facts/issues without a deny-per-read (each denied read = a wasted tool call + retry); (2) the execution guard allows scoped binaries (`rm`/`mv`/`cp`/`chmod`/`chown`/`ln`) inside the scratch dir `/tmp/orchicon`, so scratch cleanup doesn't trip the boundary; (3) `permission.task` is denied — Orchicon splices the work itself, so opencode's subagent tool (which would re-prepend a system prompt + re-carry parent history ≈ 2× context) is disabled, and the shared efficiency prompt tells workers not to delegate; (4) tool outputs are capped (`ORCHICON_MAX_TOOL_OUTPUT_BYTES`, default 128 KiB); (5) `contextfiles` caps: `MaxInlineFileBytes` 256→64 KiB + cumulative `MaxInlineContextBytes` 384 KiB. **Context-by-reference wave (delta handoff)**: (6) project/work-item context renders via `RenderManifest` — small core files (<= `ManifestInlineMaxBytes`, 4 KiB) inline (never-blind floor), larger files/dirs are a path+size manifest to read on demand (agnostic for any project shape); (7) a worker's Execution history is **delta** — full detail only for its DIRECT upstream steps (`DependsOn`), a compact `✓✗ step [iteration] — summary` index for every other prior step; (8) each step's full detail is persisted to `.orchicon/<run>/steps/<stepId>.md` (never overwritten) and `orchicon run-context <runID>` prints/greps that archive on demand. The stable prompt prefix stays byte-identical across a run's steps so KV/prompt caching still renders; AC/instructions are NEVER shrunk — only history re-embedding (the D guardrail).

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
| `container-rebuild` | Stop an instance, rebuild the image (frontend always forced), start it (usage: `make container-rebuild instance=dev\|prod`) |
| `container-up` | Start the dev single-container instance |
| `container-down` | Stop the dev single-container instance |
| `container-status` | Show single-container instance status |
| `container-logs` | Tail the dev container instance logs |
| `container-ps` | List orchicon container instances |
| **Frontend** | |
| `fe-install` | Install frontend dependencies |
| `fe-dev` | Start Vite dev server |
| `fe-build` | Build for production (`force-fe=1` always rebuilds) |
| `fe-lint` | Lint frontend |
| **Install** | |
| `install-dry-run` | Dry-run the install script (no changes made) |
| `install-uninstall` | Uninstall Orchicon via the install script |
| **Hygiene** | |
| `clean` | Clear the Go build cache (`go clean -cache -testcache -modcache`) + `bin/` |
| `cache-check` | Report the current Go build cache size |
| `clean-docker` | Prune Orchicon's dangling images + its own stopped containers (`--filter label=orchicon-instance`). **Never volumes**: it used to run `docker volume prune -f` host-wide, which removes other projects' data on any machine with more than Orchicon on it |
| **CI** | |
| `cross-compile` | Compile both shipped binaries for every release platform (linux/darwin/windows × amd64/arm64, `CGO_ENABLED=0`). Catches platform-specific breaks — see *Continuous integration* below |
| `ci-go` | The Go control-plane gate: lint → gen-check → vet → test → synth-data → rls-check → adapter-bake-guard → **cross-compile**. This is exactly what the `go-ci` workflow job runs |
| `ci` | `ci-go` + `fe-lint` + `fe-test` — the full gate |

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

### Continuous integration

The workflows live in `.github/workflows/`. `ci.yml` runs three jobs — `go-ci` (which is
`make ci-go`), `fe-lint` and `docs-check` — and the two things worth knowing are **when it runs** and
**what the Go job actually compiles**.

**It runs for pull requests into `develop` AND `main`.** The `main` entry is not decorative: a release
is cut by merging `develop` → `main`, so that is the PR whose merge produces the shipped artefacts, and
for a while it was the one PR CI never tested. The consequence was concrete — v0.4.0 was tagged and then
produced **no release at all**, because `release.yml` failed on both Windows targets with
`undefined: syscall.Kill` (a Unix-only symbol called from a file with no build constraint). Every gate
was green; the artefact could not be built. `auto-release.yml` had already created the tag by then, so
the failure arrived *after* the version existed.

**`go-ci` COMPILES EVERY PLATFORM THE RELEASE SHIPS TO, which is what catches that class of bug.**
`go build`, `go vet` and `go test` all pass on linux/amd64 for code that cannot build for Windows, so no
amount of running them on one platform would have found it. `make cross-compile` compiles the exact
release matrix — linux/darwin/windows on amd64 and arm64, `CGO_ENABLED=0`, both binaries — and it is
part of `make ci-go`, so the gate and the release cannot disagree about which platforms must build.

**`release.yml` guards itself: only tags reachable from `main` are released.** A develop version-bump
tag is skipped with a warning, so the per-merge tagging in `develop-bump.yml` can never publish a
release by accident. The release matrix additionally verifies what it is about to publish — if any leg
fails, **no GitHub Release is created**, which is the correct failure but silent at the time: the tag
exists and nothing is downloadable. If a version has a tag and no release, check `release.yml`'s run for
that tag before anything else.

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
10. **A permission ask blocks its turn** — the adapter holds the tool call on the decision, so a model can never act on an action the operator has not answered
11. **A deny is a decision, not a permission request** — no card is raised for it, and no session grant, accept entry or Fullsend can override it
12. **Silence is a denial, and the model is told which** — a timeout fails closed, but is reported as *expired* rather than as an operator refusal (the operator did not refuse, and the call will ask again)
13. **The transcript is the server's truth for what happened to an ask** — a decision is written into the turn's ledger keyed by ask id and persisted with the message, so any client (a second tab, another device, a reload) settles the same way the answering client did
14. **A mode is enforced by the adapter, not requested in a prompt** — the tool boundary for brainstorm/iteration/quick-work is applied below the model, so it holds for every adapter
15. **A permission bypass lives in memory and dies with the plane** — session grants and Fullsend are never persisted, because a bypass that survives a restart is one the operator has forgotten is on

### Styling & Conventions

- **Go**: Standard library style, `internal/` packages, pgx parameterized queries
- **TypeScript**: TanStack Query for server state, Zustand for UI state, shadcn/ui components
- **CSS**: Tailwind utility classes with **20** CSS-variable themes (10 light + 10 dark — `LIGHT_THEMES` + `DARK_THEMES` in `frontend/src/lib/themes.ts`; the default dark theme is **Teal Depths**)
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

The Cloudflare Pages project (`orchicon-site`), its `wrangler.toml` and the one-time
setup notes are deployment configuration rather than part of this repository, and are
therefore kept out of it.


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
| `ORCHICON_HTTP_EXTRA_BIND` | *(empty)* | Second HTTP bind: a concrete docker-bridge `host:port` (e.g. `172.17.0.1:8091`), so runtime containers reach a host-resident plane across the bridge. Loopback-only when empty. Never a wildcard — the plane must not be reachable from another machine. Set per instance by `scripts/container.sh plane-bind <dev\|prod>`. |
| `ORCHICON_GRPC_ADDR` | `:9090` | gRPC listen address |
| `ORCHICON_POSTGRES_DSN` | `postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable` | PostgreSQL connection string |
| `ORCHICON_NATS_URL` | `nats://localhost:4222` | NATS server URL |
| `ORCHICON_OTEL_ENDPOINT` | `localhost:4317` | OTel collector gRPC endpoint |
| `ORCHICON_GRAFANA_URL` | `http://localhost:3002` | Grafana UI URL (proxied same-origin under /grafana) |
| `ORCHICON_TEMPO_URL` | `http://localhost:3200` | Tempo query API URL |
| `ORCHICON_LOKI_URL` | `http://localhost:3100` | Loki query API URL |
| `ORCHICON_VM_URL` | `http://localhost:8428` | VictoriaMetrics query API URL |
| `ORCHICON_MODE` | `local` | Operating mode: `local` or `production` |
| `ORCHICON_DEPLOYMENT_TENANT_ID` | `tnt_dev` | The single tenant this deployment owns: the OIDC callback, embedded-OP local login, and the local-admin bootstrap all resolve logins into it; boot provisions it via the tenant seed. Must be lowercase alphanumerics plus `-`/`_` (≤63 chars); a misconfigured value fails boot. IdP identity claims are never consulted for tenant selection |
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
| `ORCHICON_COMPACT_CACHE_DISCOUNT` | `0.1` | **Deprecated / no longer used.** Cache-read weighting in the compaction gate was removed: cost is priced (cache-aware) by the provider, and the `tokens` gate counts FRESH tokens only (prompt+completion+reasoning), with cache reads excluded. Cache reads still govern spend via the cost gate, which prices them at the provider's discounted rate. |
| `ORCHICON_COMPACT_MIN_TURNS` | `2` | Minimum completed turns before the compact-on-budget-breach gate is armed (prevents compact-at-start and the compact loop) |
| `ORCHICON_COMPACT_MAX` | `1` | Max compactions per execution. The spend accumulator is cumulative, so once a budget is tripped it stays tripped — capping at 1 prevents re-collapsing the fresh post-compact summary every turn. 0 disables compaction entirely |
| `ORCHICON_FOLLOWUP_REPLY_WINDOW` | `30m` | Async follow-up reply window; the collected reply is written when it lands within this bound |
| `ORCHICON_ASK_REPLY_WINDOW` | `30m` | Ask Orchicon detached reply window; a turn that goes quiet for this bound is persisted as a timeout error message. A turn **parked on a permission/question card is exempt** — the operator is what it is waiting on, so the window re-arms and the registry's TTL sweep spares it (see §Ask Orchicon consent), and the turn ends on a decision or a stop rather than on a timer |
| `ORCHICON_ASK_TIMEOUT` | `60s` | Ask Orchicon send-accept bound: how long a turn's attempt waits for the serve to accept the sent message once subscribed. A wedged serve fails fast instead of silently queuing |
| `ORCHICON_ASK_SERVE_DOWN_GRACE` | `15s` | Ask Orchicon serve-down fast-fail: how long a turn whose serve has NEVER accepted a connection keeps retrying before the reply is persisted as a serve-unavailable error (a serve that was live earlier keeps the full reply window for restart recovery) |
| `ORCHICON_ASK_REATTACH_BACKOFF` | `2s` | Ask Orchicon pause between re-attach attempts after serve loss mid-reply (the collector retries inside the reply window) |
| `ORCHICON_ASK_STALL_NO_PROGRESS_WINDOW` | `120s` | Ask Orchicon stall monitor: no text/reasoning/step_finish/tool_use for our session within the window trips `no_progress`, aborts the serve session and fails the turn with a clear, retryable error |
| `ORCHICON_ASK_STALL_REPETITION_COUNT` | `5` | Ask Orchicon stall monitor: same tool-call signature repeated more than this many times within the repetition window trips `repetition` |
| `ORCHICON_ASK_STALL_REPETITION_WINDOW` | `300s` | Ask Orchicon stall monitor: the window over which identical tool-call signatures are counted for the repetition signal |
| `ORCHICON_ASK_TURN_MAX_AGE` | `31m` | Ask Orchicon turn-registry TTL: a turn older than this is evicted by the background sweeper (collector cancelled, serve session aborted) so no conversation can be blocked forever by a wedged collector |
| `ORCHICON_ASK_SWEEP_INTERVAL` | `1m` | Ask Orchicon turn-registry sweeper tick interval (dev/test knob) |
| `ORCHICON_PERMISSION_POLICY` | *(per-instance data dir)* | The Ask permission policy file (`deny`/`accept` lists). Read on **every** gated decision, so an edit takes effect on the next call — no restart |
| `ORCHICON_ASK_CONSENT_WAIT` | *unset — no bound* | Optional leash on a permission card; unset means a card waits for the operator indefinitely (a human deciding is not a stall, and no timer ends the wait). When set, a timeout is a **denial** (fail closed) and the model is told it *expired* rather than that the operator refused. `ORCHICON_CLAUDE_CONSENT_WAIT` is the same knob for the claude transport, and the two defaults are kept identical on purpose |
| `ORCHICON_ASK_MCP_TOOL_WEDGE_WINDOW` | `120s` | A tool call issued but never resolved within this window is treated as a wedged MCP call and the session is recycled (any activity resets it) |
| `ORCHICON_ASK_MCP_RECONNECT_ATTEMPTS` | `3` | How many times a wedged session is recycled within one turn before the turn is failed with a clear, retryable error |
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
| `ORCHICON_SESSION_REPAIR_ATTEMPTS` | `3` | Dispatch self-heal: how many runtime-container repairs (recycle + fresh serve/store) one dispatch attempts when session creation has failed for infra reasons persistently. `<1` disables. |
| `ORCHICON_SESSION_INFRA_THRESHOLD` | `2` | Dispatch self-heal: consecutive infra failures (within one dispatch) before the container is recycled — a single infra blip is retried on the same container so a healthy parallel step isn't torn down. `<2` disables container repair on the consecutive counter. |
| `ORCHICON_RECOVERING_STALL_TIMEOUT` | `15m` | Reconciler never-limbo: fail a recovering step terminal (finalizing the run, surface Retry) when it has been past this window in the recovery-dispatch gate dead-ends (no recovery row ever, or seed never resolvable). `<1s` disables. |
| `ORCHICON_MAX_TOOL_OUTPUT_BYTES` | `131072` | Context efficiency: cap a single tool's output before it is persisted/re-shown (`internal/opencode/adapter.go` `capToolOutput`). A build/test log or large listing otherwise re-enters every later turn's model context. `<1` disables the cap. |
| – (`ManifestInlineMaxBytes`) | `4096` | Context-by-reference (`contextfiles.RenderManifest`): files at or under this size are inlined into the prompt (never-blind floor); larger files/dirs become path+size manifest entries read on demand. |
| `ORCHICON_RUNTIME_POOL_CAP` | `1` | Warm pool: max clean (idle) containers kept per environment (image + project mounts). |
| `ORCHICON_RUNTIME_POOL_IDLE` | `10m` | Warm pool: how long a clean container may sit unused before it is idle-reaped. |
| `ORCHICON_RUNTIME_LEASE_MAX` | `30m` | Warm pool: how long a LEASED container's lease may stay unrenewed before it is reaped with its container (the aborted-run leak class — a live run renews every ≤30s via the adopt sweep). |
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

A fresh plane is bootstrapped by the operator creating their own admin
account: on first load, click **Sign up** and the first account becomes the
tenant admin (via the embedded IdP). The local-mode bootstrap is **opt-in**
(requires both `ORCHICON_LOCAL_ADMIN_USERNAME` and
`ORCHICON_LOCAL_ADMIN_PASSWORD` pinned); there is no built-in default
`admin`/`admin` credential.

| Task | How |
|---|---|
| First-boot admin | On first load, click **Sign up** and create the first account — it becomes the tenant admin (is_admin true, admin role bound atomically with account creation). A second sign-up is a plain `user` with no admin grant. |
| Change the admin password (you are logged in) | **Admin → Identities → Edit**: pick the identity, set the Username + New password fields in the row-edit modal and Save. This calls the admin-only `SetLocalCredential` RPC (`auth:write`); the password is argon2id-hashed at the boundary and the hash is never returned. Equivalent API call: `POST /orchicon.api.v1.AuthService/SetLocalCredential` with `{identity_id, username, password}`. |
| Lost the admin password (locked out) | Stop the plane, set `ORCHICON_LOCAL_ADMIN_RESET=1` **and** pin the new credential with both `ORCHICON_LOCAL_ADMIN_USERNAME` and `ORCHICON_LOCAL_ADMIN_PASSWORD`, start it again, sign in with the new credential, then unset the env so the next boot is normal. The reset overwrites only the credential — the admin identity and role binding are preserved. It is local mode + embedded OP only and requires the pin (no default credential fallback), so a locked-out operator always has a path back in. |
| No admin exists yet | A fresh plane has zero admins until the first sign-up (or the opt-in bootstrap) creates one. If no admin exists, the plane is in production mode (external IdP owns auth), the embedded OP is disabled, or no one has signed up yet — those planes never auto-provision a default credential. In local mode with the embedded OP enabled, sign up (first account becomes admin) or pin both `ORCHICON_LOCAL_ADMIN_*` envs. |

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
3. The version tag is bumped automatically on each merge to `develop`
   (`.github/workflows/develop-bump.yml`); `git fetch --tags` before rebuilding
4. Commit early and often with clear present-tense messages
5. Before PR: make sure the docs that describe what you changed match it — `ARCHITECTURE.md` for how the system is built, `USERGUIDE.md` for how it is used. (This step used to say "update `UPDATES.md`"; that file is the maintainer's own working inventory and is now **gitignored**, so it is not part of a contribution. Leave README.md's "Last Release Changes" section alone — it only changes when the human cuts a release.)
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
