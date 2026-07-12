# Orchicon — Architecture Vision

> **Version:** 0.1
> **Status:** Direction & design intent
> **Parent:** `Orchicon_Architecture_Design_Document_v0.1.md`

This document is the umbrella for the Orchicon design set. It fixes the
technology direction and the system topology that the other design
documents build on. Each sibling document owns a single concern and is
expected to evolve independently of this one.

Sibling documents:

- `02_Domain_Model.md`
- `03_Scheduler_and_Runtime_Design.md`
- `04_Runtime_Adapter_SDK.md`
- `05_Worker_Specification.md`
- `06_Recovery_Workflow_Engine.md`
- `07_API_Specification.md`
- `08_Event_Bus_and_Telemetry_Model.md`
- `09_Database_Schema.md`
- `10_Frontend_Architecture.md`

---

## 1. Mission

Orchicon orchestrates autonomous AI work as reliable, observable,
recoverable, and manageable systems. It does **not** execute work itself
— it schedules, governs, observes, and recovers work executed by
pluggable runtimes.

> Orchicon orchestrates. Runtimes execute.

---

## 2. Technology Direction (v0.1)

| Concern | Choice | Rationale |
|---|---|---|
| Control plane language | **Go** | Goroutine model fits a stateful reconciler; strong gRPC; single static binary; same family as Kubernetes controller patterns |
| API contract | **Protobuf + Connect** (bufbuild) | One schema generates gRPC, Connect-REST, and TS clients; bidirectional streaming for telemetry/control |
| Database | **PostgreSQL 16** (`pgx`) | Source of truth, transactional outbox, advisory locks for leader election |
| Event bus | **NATS JetStream** | Lightweight durable streaming; no Kafka ops burden; covers control + telemetry fan-out |
| Telemetry | **SigNoz (ClickHouse + OTel-native UI)** — dedicated, fully separated telemetry infrastructure | Open-source (Apache 2.0), OTel-native ingest, unified query model; runs as its own infrastructure (separate from control-plane Postgres), independently scalable and operable |
| Object storage | **`BlobStore` abstraction** (S3-compatible + local-filesystem) | Thin interface; `S3BlobStore` for cloud, `LocalFilesystemBlobStore` for fully-local/single-machine deployments (production-viable, not just dev); zero cloud dependency for local installs |
| Runtime transport | **gRPC sidecar per runtime** | Strongly typed, bidirectional streaming, language-agnostic adapters |
| Frontend | **TypeScript + React + Vite** | Generated API client from Buf schema; realtime via WebSocket subscriptions |
| Migrations | **Atlas** (declarative) | Reviewable schema diff; integrates with CI |
| Auth | **OIDC + API keys + RBAC** | Enterprise SSO via OIDC; machine-to-machine via scoped API keys |
| Policy engine | **Rego (Open Policy Agent)** | Declarative, versioned, scoppable; Tier 1 decision-point policy only in v0.1; Go hooks deferred to v0.2 |
| AI Gateway | **Embedded in control plane binary** (v0.1) | Single binary, one deploy; structured as an internal package extractable to a separate deployable in v0.2+ if scale demands |
| Leader election | **Postgres advisory locks** (canonical) | Postgres is the source of truth, so it's also the lock authority; no etcd, no NATS-based lease |
| Packaging | **Single Go binary** + sidecars | Control plane ships as one binary; runtimes are pluggable sidecars; fully local (no cloud) is a supported deployment |

### Out of scope for v0.1

- A native Orchicon runtime (runtimes remain pluggable adapters).
- Multi-tenant isolation beyond project-scoped RBAC.
- Federation across Orchicon clusters.
- A custom LLM gateway UI beyond provider routing/accounting.

---

## 3. System Topology

```
                       ┌────────────────────────────┐
                       │      Frontend (React)       │
                       │  generated Connect-ES client │
                       └─────────────┬──────────────┘
                                     │ Connect (gRPC/REST/WS)
                                     ▼
             ┌──────────────────────────────────────────────────┐
             │            Orchicon Control Plane (Go)            │
             │                                                   │
             │  API layer  │  Scheduler/Reconciler  │  Recovery   │
             │  Auth/RBAC  │  Worker Manager        │   Engine    │
             │  AI Gateway │  Policy Engine         │  Telemetry  │
             │  BlobStore  │                        │  Webhooks   │
             └──┬──────────┬──┬──────────────────────┬──────────┘
                │          │  │                      │
       ┌────────▼──────┐ ┌─▼──▼─────────┐ ┌──────────▼──────────────┐
       │  PostgreSQL   │ │ NATS JetStream │ │  Telemetry Infra        │
       │  (source of   │ │ (events/durab- │ │  (fully separated)      │
       │   truth, RLS, │ │   le streams)  │ │  OTel Collector →       │
       │   advisory    │ │                │ │  SigNoz (ClickHouse)    │
       │   locks)      │ │                │ │  + BlobStore (S3/Local) │
       └───────────────┘ └────────────────┘ └─────────────────────────┘
                              │
              ┌───────────────┼───────────────────┐
              ▼               ▼                   ▼
      ┌──────────────┐ ┌──────────────┐  ┌──────────────────┐
      │ Runtime      │ │ Runtime     │  │  Runtime          │
      │ Adapter:     │ │ Adapter:    │  │  Adapter: future │
      │ OpenCode     │ │ (future)    │  │  (Claude/Codex)  │
      └──────┬───────┘ └─────────────┘  └──────────────────┘
             │ gRPC
      ┌──────▼───────┐
      │  OpenCode    │
      │  (executes)  │
      └──────────────┘
```

### Control plane vs. data plane vs. telemetry plane

- **Control plane**: API, scheduler, reconciler, recovery engine,
  policy engine, AI gateway, webhook dispatcher, BlobStore interface.
  Stateless processes scaled horizontally; state lives in Postgres +
  NATS + BlobStore.
- **Data plane**: runtime adapters + runtimes. Each adapter is a sidecar
  process owned by a single worker execution; it terminates when the
  execution terminates.
- **Telemetry plane**: fully separated from the control plane. OTel
  collector → SigNoz/ClickHouse, deployed and scaled independently. The
  control plane emits telemetry into this plane but does not own it.

### Deployment modes

- **Fully local (v0.1 supported)**: control plane + Postgres + NATS +
  SigNoz/ClickHouse + BlobStore (local filesystem) all on one machine.
  No cloud dependency. This is the day-1 deployment for single-user or
  small-team use.
- **Cloud/HA**: control plane replicas + managed Postgres + NATS
  cluster + SigNoz/ClickHouse cluster + S3-compatible BlobStore.
  Horizontal scaling with leader election.

---

## 4. Design Principles → Mechanisms

Each principle in the parent document maps to a concrete mechanism so
that the principle is enforceable rather than aspirational.

| Principle | Mechanism |
|---|---|
| Runtime Agnostic | `RuntimeAdapter` gRPC contract; capabilities negotiation per execution |
| Project-Centric | Projects are the top-level tenant of state; all entities FK to a project |
| State Driven | Reconciler continuously diffs desired vs. actual state; idempotent applies |
| Observable by Default | Every state transition emits an OTel span + a NATS event; no silent paths |
| Recoverable | Checkpoints written to Postgres + object store; recovery workflows replay from checkpoints |
| Self-Healing | Health monitor triggers Recovery Workflow Engine on stall/health-budget breach |
| Composable | Workers, workflows, and policies are versioned, referenced resources |
| Human Governed | Policies are first-class; approval gates are explicit workflow steps |
| Extensible | Adapters, providers, and storage sinks register via interfaces |

---

## 5. Reconciliation Model

Orchicon is built around a Kubernetes-style reconcile loop, not a
request/response executor. Every mutable object has:

- a **desired state** (user-asserted, stored in Postgres),
- an **observed state** (reported by adapters/telemetry),
- a **status** the reconciler writes back.

Reconcilers are keyed by object kind (Project, Worker, Task, Workflow,
Policy). Each runs as a goroutine pool consuming a per-object work queue
de-duplicated by object ID. Leadership is elected per reconciler kind
via Postgres advisory locks so multiple control-plane replicas can run
safely without etcd.

This is intentionally lighter than `controller-runtime`: no informer
caches (Postgres is the cache), no CRDs, no k8s dependency.

---

## 6. Cross-Cutting Invariants

These hold across every sibling document and must not be silently
violated:

1. **Projects are the trust boundary.** No cross-project reads or writes
   except via explicit, audited federation (deferred to v0.2+).
2. **Postgres is the source of truth.** NATS, caches, and telemetry
   stores are derivable; losing them must not corrupt control state.
3. **Every mutation emits an event** through the transactional outbox.
   No "fire and forget" side effects from the API layer.
4. **Adapters never hold durable state.** Any state an adapter needs to
   survive a crash is checkpointed through the control plane.
5. **Telemetry is append-only.** No mutation of historical signals;
   corrections are new events.
6. **Policies are evaluated before dispatch and after completion.** No
   execution path bypasses the policy engine.
7. **Recovery is opt-out per workflow, not opt-in.** A workflow must
   explicitly disable recovery; the default is to recover.

---

## 7. v0.1 Scope

The first deliverable version of Orchicon supports:

- One runtime adapter: **OpenCode**.
- One project type, one worker model, one workflow model.
- The default recovery workflow (capture → summarize → review → resume).
- REST + gRPC + WebSocket API surface for all first-class entities.
- Telemetry: task/worker/runtime/cost signals with OTel export.
- Single-region, single-cluster deployment. HA via replicas + leader
  election, not geographic replication.

Explicitly deferred: multi-region, federation, native runtime, custom
LLM training, in-platform IDE.

---

## 8. Resolved Decisions (v0.1)

- **AI Gateway**: embedded in the control plane binary for v0.1;
  structured as an internal package extractable to a separate
  deployable in v0.2+ if scale demands.
- **Object storage**: `BlobStore` abstraction with `S3BlobStore`
  (cloud) and `LocalFilesystemBlobStore` (fully-local deployments).
  Local is production-viable, not just a dev sink — the system must run
  end-to-end with zero cloud dependency on a single machine.
- **Telemetry infrastructure**: fully separated from the control plane
  from day one. SigNoz/ClickHouse + OTel collector run as their own
  infrastructure, independently scalable and operable. The control plane
  emits into this plane but does not own it.
- **Leader election**: Postgres advisory locks are the canonical
  mechanism. No etcd, no NATS-based lease. Postgres is the source of
  truth, so it is also the lock authority.

---

## 9. Development Workflow

### Git

- **Repo**: https://github.com/beardedparrott/Orchicon.git
- **No commits directly to `main`.** Every major change gets a branch.
- Branch naming: `<type>/<short-description>`
  (e.g. `feat/project-crud`, `fix/outbox-relay-dedup`).
- Types: `feat`, `fix`, `chore`, `refactor`, `docs`, `test`.
- Commit early and often; stage only relevant files.
- Push branches and create PRs via `gh pr create`.
- See `AGENTS.md` at repo root for the full agent-facing workflow.

### Build order: vertical slices

Development proceeds in vertical slices — each phase delivers a working
backend + frontend increment, not "all backend, then all frontend."

1. **Foundation**: Go module, proto schema, Atlas migrations, Docker
   Compose, Vite+React scaffold.
2. **Projects slice**: Project CRUD (full stack) + frontend project list
   and detail views.
3. **Realtime + infrastructure**: Outbox relay, reconciler framework,
   OTel, frontend streaming (`useStream` hook).
4. **Workers + WorkItems**: Worker versioning, WorkItem hierarchy,
   dependencies + frontend catalog, tree/board, dependency graph.
5. **Scheduling + adapters**: TaskReconciler, dispatch, OpenCode adapter
   + frontend execution live view.
6. **Workflows**: Workflow CRUD, step DAG, runs + frontend visual
   drag-and-drop editor (React Flow).
7. **Recovery + Policy**: Recovery Engine, Rego Policy Engine + frontend
   recovery timeline, policy editor.
8. **Telemetry + Cost**: OTel pipeline, SigNoz integration, cost
   attribution + frontend seamless SigNoz embedding, cost explorer.
9. **Auth + Webhooks + Polish**: OIDC, API keys, RBAC, webhooks,
   edit locks + frontend auth flow, end-to-end integration.
