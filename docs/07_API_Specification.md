# Orchicon — API Specification (REST / gRPC / WebSocket)

> **Version:** 0.1
> **Status:** Direction & design intent
> **Parent:** `01_Architecture_Vision.md`

This document fixes the API platform direction: contract, transport,
streaming, auth, versioning, and the service surface. It is the source
of truth for what the API exposes; concrete message definitions live in
the Protobuf schema repository and are generated, not hand-written
here.

> **Principle:** The UI is one client of the API. Everything available
> in the UI is available through the API. Everything external systems
> need to drive Orchicon is available through the API.

---

## 1. Design Intent

- **One schema, three transports.** A single Protobuf schema generates
  gRPC, Connect-REST, and TypeScript clients. No hand-maintained REST
  surface.
- **Streaming is first-class.** Telemetry, execution events, and
  recovery updates are streams, not polls.
- **API and UI share a contract.** The frontend never hand-writes
  endpoints; it imports a generated client.
- **Auth is uniform.** Same identity model for humans (OIDC) and
  machines (API keys); same RBAC for both.

---

## 2. Contract & Transports

- **Schema format**: Protobuf v3, managed with **Buf**.
- **Transports**:
  - **gRPC** — for high-throughput machine clients and adapters.
  - **Connect** (Connect-ES / connect-go) — HTTP/JSON + HTTP/2
    streaming; the default for browsers and curl-style clients.
    Connect exposes the same RPCs as gRPC from one schema.
  - **WebSocket** — for frontend realtime subscriptions layered over
    the same RPC service via streaming RPCs (not a separate WS API).
- **Streaming RPCs** use server-streaming (telemetry, event tails) and
  bidirectional streaming (adapter execution, in-flight control).
- **No REST-only endpoints.** If it isn't in the schema, it doesn't
  exist.

### 2.1 Why Connect over raw REST

- One schema → gRPC + JSON + streaming, with no duplicate OpenAPI.
- Frontend uses Connect-ES, the same client library as browsers.
- Field-mask, pagination, and error model come from the schema, not
  ad hoc per endpoint.

---

## 3. Service Surface (v0.1)

Services are grouped by domain. Each service owns CRUD + lifecycle +
streaming for its entity.

### 3.1 `ProjectService`
- `CreateProject`, `GetProject`, `ListProjects`, `UpdateProject`,
  `ArchiveProject`, `DeleteProject`
- `PauseProject`, `ResumeProject`
- `StreamProjectEvents` (server-stream) — lifecycle, budget, health

### 3.2 `WorkItemService`
- `CreateWorkItem`, `GetWorkItem`, `ListWorkItems`, `UpdateWorkItem`
- `AddDependency`, `RemoveDependency`, `GetDependencyGraph`
- `ReorderWorkItems`, `AssignWorker`, `UnassignWorker`
- `StreamWorkItemEvents` (server-stream)

### 3.3 `WorkerService`
- `CreateWorker`, `PublishWorkerVersion`, `DeprecateWorker`,
  `RetireWorker`
- `GetWorker`, `ListWorkers`, `ListWorkerVersions`
- `StreamWorkerExecutions` (server-stream) — in-flight + recent
- `AcquireEditLock`, `ReleaseEditLock`, `GetEditLock` — explicit edit
  lock for the visual Worker editor (same mechanism as WorkflowService)

### 3.4 `WorkflowService`
- `CreateWorkflow`, `PublishWorkflow`, `GetWorkflow`, `ListWorkflows`
- `StartWorkflow`, `AbortWorkflow`
- `StreamWorkflowEvents` (server-stream) — step transitions
- `AcquireEditLock`, `ReleaseEditLock`, `GetEditLock` — explicit edit
  lock with TTL + heartbeat renewal; prevents concurrent edits. Other
  users see "currently being edited by [user]" and can view read-only
  or request handoff. Lock expires automatically on disconnect.

### 3.5 `PolicyService`
- `CreatePolicy`, `PublishPolicy`, `SupersedePolicy`
- `GetPolicy`, `ListPolicies`
- `EvaluatePolicy` (unary, dry-run against a decision point + input)
- `ExplainDecision` (unary, returns the Rego evaluation trace for a
  past `policy.evaluated` event by `trace_id` or `decision_id`)

> **Tier 1 (decision-point) policy** is the v0.1 baseline, always-on,
> Rego-only. **Tier 2 (per-tool-call gating)** is opt-in per Worker
> (gated tools: `terminal`, `web_fetch`, `git` in v0.1) and surfaces
> through `ExecutionService.ApproveToolCall`. Go hooks deferred to v0.2.

### 3.6 `RecoveryService`
- `GetRecoveryWorkflow`, `UpdateRecoveryWorkflow`
- `TriggerRecovery`, `CancelRecovery`
- `StreamRecoveryEvents` (server-stream)
- `ApproveContinuationPlan`, `RejectContinuationPlan`

### 3.7 `RuntimeAdapterService`
- `RegisterAdapter`, `DeregisterAdapter`, `ListAdapters`
- `GetAdapterCapabilities`

### 3.8 `ExecutionService`
- `GetExecution`, `ListExecutions`, `StreamExecutionEvents`
  (server-stream — telemetry, tool calls, health)
- `PauseExecution`, `ResumeExecution`, `CancelExecution`,
  `CheckpointNow`
- `ApproveToolCall` (Tier 2: when a Worker's `gated_tools` declares
  `terminal` / `web_fetch` / `git`, the adapter emits an
  `ApprovalRequest` for each call in that category; this RPC returns
  the human or Policy-derived decision)

### 3.9 `TelemetryService`
- `QueryTraces`, `QueryMetrics`, `QueryLogs` (proxy to SigNoz/ClickHouse
  with tenant-scoped filters)
- `StreamTelemetry` (server-stream, multi-subscription)

### 3.10 `AIGatewayService`
- `ListProviders`, `GetUsage`, `GetCost`
- `StreamUsageEvents`

### 3.11 `WebhookService`
- `CreateSubscription`, `GetSubscription`, `ListSubscriptions`,
  `DeleteSubscription`
- `TestSubscription` (sends a test event to the registered endpoint)
- `StreamSubscriptionDeliveries` (server-stream) — delivery attempts,
  successes, failures, dead-lettered events
- Subscription shape: filter (event-type + tenant/project scope),
  target URL, secret (for HMAC signing), retry config, dead-letter
  config. The dispatcher is a NATS consumer that POSTs to registered
  endpoints with exponential backoff; dead-lettered events are
  queryable and replayable.

### 3.12 `AuthService`
- `CreateApiKey`, `RevokeApiKey`, `RotateApiKey`
- `GetIdentity`, `ListEntitlements`
- (OIDC login is handled out-of-band via the IdP; this service manages
  issued tokens/keys and entitlements.)

---

## 4. Streaming Semantics

- **Server-stream RPCs** are long-lived; clients subscribe with a
  filter (object id, event kinds, severity) and receive a sequence
  until cancel or stream close.
- Events carry monotonically-increasing sequence numbers per stream;
  clients may resume from a sequence number after reconnect.
- The backend fan-outs from NATS subjects (see
  `08_Event_Bus_and_Telemetry_Model.md`) — streams are thin projections
  of the durable bus, not a separate path.
- **Backpressure**: slow consumers receive a configurable policy
  (`drop_oldest` | `block` | `close`); default is `drop_oldest` with a
  high-water mark.

---

## 5. Common Conventions

### 5.1 Naming
- RPCs are verbs; resources are nouns.
- Field names are `snake_case` in proto, mapped to `camelCase` in JSON
  by Connect automatically.
- Resources use ULIDs as IDs; slugs are URL-safe aliases where stable.

### 5.2 Pagination
- Cursor-based (`page_token`, `next_page_token`), never offset.
- Max page size enforced server-side (default 100, max 1000).

### 5.3 Errors
- `google.rpc.Status` with canonical codes; rich details via
  `google.rpc.ErrorInfo` and a custom `OrchiconError` carrying
  `code`, `retryable`, `trace_id`.
- Errors include an OTel `trace_id` so UI/logs/traces join on one id.

### 5.4 Field masks
- All `Update*` RPCs accept a `FieldMask`; partial updates are the norm.

### 5.5 Idempotency
- All mutating RPCs accept an optional `request_id`; the server
  deduplicates for a TTL window and returns the prior response.

### 5.6 Time
- All timestamps are `google.protobuf.Timestamp` in UTC; durations are
  `google.protobuf.Duration`.

---

## 6. Authentication & Authorization

### 6.1 Identity
- **Humans**: OIDC (SSO). The control plane validates ID tokens and
  issues short-lived Orchicon access tokens.
- **Machines**: API keys (hashed at rest) with scoped entitlements.
- **Adapters**: mTLS + adapter-kind-specific credentials; tokens scoped
  to the `RuntimeAdapterService` and `ExecutionService` only.

### 6.2 RBAC
- Entitlements are `resource:action` pairs (e.g.
  `project:create`, `worker:publish`, `policy:supersede`).
- Entitlements attach to identities via Roles; Roles are tenant-scoped
  with optional project-scoping.
- Policy Engine evaluates both **resource-level RBAC** (may this
  identity touch this resource?) and **domain Policies** (may this
  action proceed given state?) — these are distinct checks at distinct
  points.

### 6.3 Per-call enforcement
- Every RPC passes through auth middleware → RBAC check → Policy
  evaluation (where applicable) → handler.
- The middleware emits an OTel span with identity + entitlements
  decided, so every API call is auditable end-to-end.

---

## 7. Versioning

- **API version**: per-service, in the Protobuf package
  (e.g. `orchicon.worker.v1`). Breaking changes ship `v2` packages;
  both run concurrently during migration.
- **Stability levels**: `experimental` → `stable` → `frozen`.
  `frozen` services guarantee no breaking changes ever.
- **Resource versions** (Worker, Workflow, Policy) are independent of
  API version and tracked per-entity (see Domain Model).
- Deprecation follows a documented timeline: announce → dual-run →
  sunset. No silent removals.

---

## 8. Rate Limiting & Quotas

- Per-tenant rate limits on mutating RPCs; per-identity limits on
  streaming RPCs (concurrent streams + messages/sec).
- Burst budgets refill with token bucket; defaults are generous for
  humans, tighter for unattended machines.
- Rate-limit decisions are visible in response headers and as OTel
  metrics; never silently dropped.

---

## 9. Cross-Cutting Invariants

1. No business logic is reachable except through the schema-defined
   RPCs. No backdoors, no admin RPCs undocumented.
2. Every mutating RPC writes to the transactional outbox; clients see
   consistent state via the resulting events, never via direct reads of
   a half-applied transaction.
3. Every RPC has an OTel span rooted in the caller's trace context;
   traces cross the API → reconciler → adapter boundary.
4. Streaming RPCs are projections of the durable event bus, not a
   separate source of truth — a disconnected client never loses state
   permanently.
5. The same schema generates the frontend client; the UI cannot drift
   from the API by construction.

---

## 10. Resolved Decisions (v0.1)

- **GraphQL**: Connect only for v0.1. The API surface is Connect
  (gRPC + JSON + streaming) with field masks for partial updates. No
  GraphQL projection — Connect already provides typed clients,
  streaming, and partial updates without doubling the API surface.
- **Webhooks**: first-class in v0.1. Modeled as a `Subscription` entity
  (filter + delivery target + retry config + dead-letter). The outbox
  relay already publishes events; a webhook dispatcher is a thin NATS
  consumer that POSTs to registered endpoints with exponential backoff
  and dead-letter. This makes Orchicon a real API platform from day
  one — CI/CD, dashboards, and other AI systems can subscribe without
  NATS.
- **Adapter-side RPCs schema placement**: separate package. The
  `RuntimeAdapterService` lives in `orchicon.adapter.v1`, separate from
  the public API's `orchicon.api.v1`. Different audience (adapter
  authors vs. API clients), different stability guarantees, different
  auth model (mTLS vs. OIDC/API keys).
