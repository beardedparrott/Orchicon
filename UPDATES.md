# UPDATES.md

> Track record of what has been shipped, phase-by-phase.
> Read this before starting any work to understand the current state.

| # | Phase | Status | What landed |
|---|---|---|---|
| 1 | Foundation | done | Go module skeleton, Protobuf schema, Connect codegen, Atlas migrations, Docker Compose (PG+NATS+SigNoz+OTel), Makefile, Vite+React+tailwind shell |
| 2 | Projects slice | done | Full-stack Project CRUD: handler, data-access layer (pgx + tenant scoping), outbox relay, frontend list/detail/create forms |
| 3 | Realtime + infra | done | `orchicon dev` subcommand, outbox metrics, reconciler framework, OTel pipeline, NATS-streamed events, `useStream` hook with reconnect |
| 4 | Workers + WorkItems | done | Worker lifecycle service, WorkItem CRUD + DAG with cycle rejection, edit locks, frontend catalog + Kanban + dependency graph |
| 5 | Scheduling + adapters | done | TaskReconciler, RuntimeAdapterService + bridge, ExecutionService with streaming + controls, OpenCode CLI adapter, execution live view |
| 6 | Workflows | done | Workflow CRUD + versions, WorkflowReconciler (step DAG progression, gate eval, task handoff), React Flow visual editor, run view with live overlay |
| 7 | Recovery + Policy | done | Policy service + Rego engine (OPA v1), RecoveryService + RecoveryReconciler (6-step flow, auto-relax, L1→L3 escalation), frontend policy editor + recovery timeline |
| 8 | Telemetry + Cost | done | `correlation_id` propagation, TelemetryService (SigNoz proxy), AI Gateway usage dual-write, cost explorer with drill-down, embedded SigNoz UI |
| 9 | Auth + Webhooks + Polish | done | OIDC auth (dev + production), API key hashing, RBAC interceptor, webhook dispatcher with HMAC + backoff, BlobStore (local + S3), admin views |
| 10 | Project context files | done | `project_dir` + `context_files`, FileBrowser component, `ListProjectFiles` RPC, file injection into composite prompt |
| 11 | Feature adds | active | Ongoing feature additions for gaps identified after initial build-out |
| 12 | Bug fixes | active | Ongoing bug fixes and polish |
| 13 | Polish: docs + website | done | README and landing page overhaul (--clean flag, commands, installed files, custom LICENSE). AGENTS.md restructured with concise Git Workflow, Phases, UPDATES.md tracking. Playwright MCP added for browser testing. |
| 14 | Telemetry fixes | done | SigNoz iframe now conditional (avoids app-in-frame when degraded). Telemetry hooks poll every 10s while degraded. Cost Explorer: fixed rollup enum, per-task API lookups for names, working drill-down. Credits tab with provider/model spend. /signoz reverse proxy in Go server. |
| 15 | Telemetry root cause | done | Rewrote SigNozClient to query ClickHouse directly via SQL (bypasses SigNoz v0.132 query-service which uses a different API with mandatory auth). Uses ClickHouse HTTP interface at port 8123 with JSONEachRow format. |
