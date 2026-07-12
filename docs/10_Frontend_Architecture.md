# Orchicon — Frontend Architecture

> **Version:** 0.1
> **Status:** Direction & design intent
> **Parent:** `01_Architecture_Vision.md`

This document fixes the frontend direction: stack, contract with the
backend, realtime model, view organization, and the invariants that keep
the UI a thin client of the API rather than a parallel source of truth.

> **Principle (from the parent doc):** The UI is simply one client of
> the API. Everything available in the UI is also available through the
> API.

---

## 1. Design Intent

- **Contract-driven.** The frontend imports a generated client from the
  Buf/Protobuf schema. No hand-written endpoint URLs, no parallel type
  definitions.
- **Realtime by default.** Live state (executions, recovery, telemetry)
  arrives over streaming RPCs; the UI does not poll for state it could
  subscribe to.
- **Thin client.** All business logic, policy, and state authority live
  in the control plane. The frontend renders and requests; it does not
  decide.
- **Composable UI.** Workers, workflows, policies are drag-and-drop
  building blocks per the parent document's vision; the UI primitives
  mirror the domain model.
- **Observable UX.** The frontend emits its own OTel spans
  (interaction, error, latency) into the same backend telemetry
  pipeline — the UI is itself a first-class observability surface.

---

## 2. Technology Stack

| Concern | Choice | Rationale |
|---|---|---|
| Language | **TypeScript** | Same language as the OpenCode adapter ecosystem; type-safe client |
| UI framework | **React 18+** | Mature, large component ecosystem, fits streaming UX |
| Build tool | **Vite** | Fast HMR, modern defaults, simple config |
| Routing | **TanStack Router** | Type-safe, file-based, data-loading built in |
| Data fetching | **TanStack Query** + **Connect-ES** | Cache, invalidation, optimistic updates; generated RPC client |
| Realtime | **server-stream RPCs** via Connect-ES | Same schema as REST; one subscription model |
| Styling | **Tailwind CSS** + **shadcn/ui** | Composable, no heavy framework lock-in, accessible primitives |
| State | TanStack Query (server state) + **Zustand** (UI-only state) | Server state stays in the cache; UI state stays local |
| Forms | **React Hook Form** + **Zod** | Schema-first validation; Zod schemas can derive from the proto types |
| Charts/telemetry | **visx** or **ECharts** for custom views; **SigNoz embedded** for raw exploration | SigNoz UI for traces/metrics/logs; custom charts for Orchicon-specific cost + execution summaries |
| Graphs (DAG, dep tree, workflow editor) | **React Flow** | Full drag-and-drop workflow editor + dependency graph visualizer (core v0.1 feature) |
| Test | **Vitest** + **Playwright** | Unit + e2e |
| OTel | **@opentelemetry/web** | Spans exported to the OTel collector → SigNoz/ClickHouse |

---

## 3. Contract with the Backend

- The Buf schema is the single source of truth.
- A codegen step (`buf generate`) emits:
  - **TypeScript message types** for every Protobuf message.
  - **Connect-ES service clients** for every RPC service.
- The frontend never hand-writes `fetch` calls to the backend. Every
  call goes through a generated client.
- Zod schemas are derived from the generated types (or hand-written to
  match) so form validation cannot drift from the API contract.
- Field-mask-based partial updates are first-class in the generated
  client; the UI uses them for every edit.

### 3.1 Codegen pipeline
```
proto/ ──buf generate──► src/api/generated/  (committed)
                              │
                              ├── clients/*.ts   (Connect-ES services)
                              └── types/*.ts     (message types)
```
Generated code is committed (not generated at build) for fast cold
starts and reliable diffs in PRs.

---

## 4. Realtime Model

The UI uses **server-streaming RPCs** from the same schema as the REST
surface. There is no separate WebSocket API to maintain.

- A small `useStream(name, filter)` hook wraps Connect-ES server-stream
  clients with:
  - automatic reconnect with exponential backoff,
  - resume from last sequence number (server supports this per
    `07_API_Specification.md` §4),
  - backpressure-aware buffering (drop-oldest for telemetry, block for
    control events).
- Subscriptions are scoped to the active view: navigating away
  unsubscribes. There is no global firehose.
- A **deduplication layer** drops events already seen (by `event_id`)
  so reconnects never double-apply state.

### 4.1 Stream → view mapping

| View | Stream | Notes |
|---|---|---|
| Project dashboard | `StreamProjectEvents` | lifecycle, budget, health |
| Task detail | `StreamWorkItemEvents` + `StreamExecutionEvents` | merged into one timeline |
| Execution live view | `StreamExecutionEvents` | telemetry, tool calls, file diffs |
| Recovery timeline | `StreamRecoveryEvents` | collapsed levels, expandable |
| Workflow editor (running) | `StreamWorkflowEvents` | step transitions |

---

## 5. View Organization

The app is organized by domain, mirroring the API services:

- **Projects** — list, detail, create/edit, pause/resume, budget
  envelope, work hierarchy tree.
- **Work Items** — Kanban + tree views, dependency graph (React Flow),
  task detail with execution timeline.
- **Workers** — catalog, version history, drag-and-drop onto tasks,
  permissions editor, budget overrides.
- **Workflows** — **full visual drag-and-drop editor** (React Flow):
  step palette (draggable Worker tiles, gate nodes, decision nodes),
  canvas with wired connections, inline property editing, undo/redo,
  validation. Run view overlays live step transitions on the same
  canvas. This is a core v0.1 feature, not deferred.
- **Policies** — list, editor (declarative rule UI), decision log
  (with trace links).
- **Recovery** — rich recovery timeline view with full detail per
  event (why/what/how/where/when), continuation plan approval/rejection,
  escalation levels with expandable sections. Not just "recovery
  happened" — a full narrative of the recovery arc with drill-down to
  span-level traces.
- **Executions** — live view with tool calls, file diffs, token/cost
  counters, health state, manual pause/resume/cancel.
- **Telemetry** — seamlessly embedded SigNoz UI for raw
  traces/metrics/logs exploration (same auth, same visual language,
  integrated within the Orchicon shell — not a separate tool launch),
  plus custom views for cost explorer and execution summaries.
  cost explorer.
- **Adapters** — registry, capabilities, health, deregister.
- **Admin** — tenants, identities, roles, API keys, audit log.

### 5.1 Composition primitives

To support drag-and-drop reuse of Workers across projects:

- A **WorkerCard** is a draggable tile representing a published Worker
  version; dropping it onto a Task or Workflow step creates a reference
  (not a copy).
- A **PolicyChip** is similarly draggable; policies attach by reference.
- These primitives operate on API references (`worker_id+version`,
  `policy_id+version`); the UI never duplicates server state.

---

## 6. State Management

- **Server state** lives in TanStack Query caches, keyed by resource.
  Invalidations follow mutations and arrive via stream events (an event
  for resource X invalidates the X query).
- **UI-only state** (open panels, selected filters, editor drafts)
  lives in Zustand stores scoped to the view.
- **Draft state** (unsaved form edits, workflow editor in progress) is
  local and explicit; the UI never silently diverges from server state.
  Save = explicit commit via mutation; cancel = discard.
- **Optimistic updates** are used sparingly, only for trivial,
  reversible changes (e.g. reorder); status transitions are never
  optimistic — they reflect server confirmation.

---

## 7. Auth & Session

- **OIDC login** redirects to the IdP; on return, a short-lived
  Orchicon access token is stored in memory (and a refresh token in an
  HttpOnly secure cookie).
- API keys are managed in Admin and used only for headless/CI clients;
  the browser never stores long-lived API keys.
- Token refresh is transparent; session expiry surfaces a re-auth
  prompt, not an error.
- RBAC gates UI affordances: actions the identity cannot perform are
  hidden or disabled, never silently failing on click.

---

## 8. Telemetry from the UI

- The frontend initializes an OTel tracer + meter; spans are exported to
  the same OTLP collector as the backend.
- Automatic spans: route transitions, RPC calls (with the same
  `trace_id` propagated to the backend via Connect headers).
- Custom spans: drag-and-drop composition, workflow editor saves,
  long-running interactions.
- Errors are captured as OTel exceptions AND as a user-facing error
  boundary with a trace link for support.

This closes the loop: a user-reported issue carries a `trace_id` that
joins the frontend interaction, the API call, the reconciler, the
adapter, and the AI Gateway.

---

## 9. Performance & Bundling

- Route-based code splitting (TanStack Router handles this).
- Streaming views are isolated; a stuck stream on one view does not
  block others.
- Virtualized lists for work item trees and telemetry tables.
- Generated client is tree-shakeable; only used services are bundled.
- Vite dev server proxies to the backend; in production, the SPA is
  served by the control plane binary (single deployable) or a CDN with
  API calls to the control plane.

---

## 10. Cross-Cutting Invariants

1. No business logic in the frontend. Policy, scheduling, and recovery
   decisions are made server-side; the UI reflects them.
2. No hand-written API URLs. Every call goes through a generated
   client; the schema is the contract.
3. No optimistic status transitions. Mutations reflect server-confirmed
   state; only reversible cosmetic updates are optimistic.
4. Every streaming view degrades gracefully: a closed stream shows
   "disconnected, retrying" without losing prior state.
5. RBAC is enforced server-side; UI gating is a UX convenience, never
   a security boundary.
6. The frontend emits OTel into the same pipeline as the backend; the
   UI is observable, not a black box.

---

## 11. Resolved Decisions (v0.1)

- **SSR / framework**: pure SPA (Vite + React). No server-side
  rendering, no meta-framework. The control plane serves the built
  assets (or a CDN does). SSR adds a Node server and hydration
  complexity that an internal platform UI doesn't need.
- **Mobile**: responsive web only for v0.1. Dashboards and execution
  views are desktop-first; approvals and status checks work on mobile.
  Native mobile client deferred.
- **Workflow editor fidelity**: **full visual drag-and-drop editor in
  v0.1.** A React Flow-based editor where users drag Workers onto a
  canvas, wire steps together visually, and edit properties inline.
  This is a core feature of the platform's promise (drag-and-drop
  composition of AI workers); shortchanging it would undermine the
  product vision. Includes: drag-and-drop, canvas state management,
  inline property editing, undo/redo, and validation. This is a
  significant frontend investment but central to Orchicon's value
  proposition.
- **Telemetry dashboard**: embed SigNoz for raw traces/metrics/logs
  exploration, **seamlessly integrated** — it must feel like part of
  the Orchicon platform, not "launching some other tool." Same auth
  (SSO passthrough or token-bridged), same visual language (consistent
  navigation, theming where possible), embedded within the Orchicon
  UI shell (iframe or component-level integration). Orchicon-specific
  views (cost explorer, execution summaries, recovery timelines, policy
  decision logs) are built custom on visx/ECharts because they're
  domain-specific. The user's experience is one platform, not two.
- **Collaborative editing**: last-write-wins with server arbitration
  for v0.1, **plus an explicit edit lock**. When a user begins editing
  a Workflow or Worker, the server issues an edit lock (with a TTL and
  heartbeat renewal). Other users see "This workflow is currently being
  edited by [user]" and can view read-only or request handoff. If the
  lock holder disconnects, the lock expires after the TTL and the
  resource becomes editable again. No CRDTs or real-time co-editing in
  v0.1; the lock prevents concurrent edits from occurring in the first
  place rather than resolving conflicts after the fact.
