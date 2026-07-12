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

**v0.1 — design phase.** Architecture documents are direction-level;
implementation has not started.

## License

TBD
