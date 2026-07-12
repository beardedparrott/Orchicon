# AGENTS.md — Development Guidelines for AI Agents

> This file is the entry point for any AI agent working on Orchicon.
> Read it before making any changes.

## Project

- **Repo**: https://github.com/beardedparrott/Orchicon.git
- **Language**: Go (control plane) + TypeScript (frontend)
- **Design docs**: `docs/` — read the relevant doc before touching a subsystem

## Git Workflow

### Branching

- **Every major change gets a branch.** No commits directly to `main`.
- Branch naming: `<type>/<short-description>` (e.g. `feat/project-crud`,
  `fix/outbox-relay-dedup`, `chore/docker-compose-setup`).
- Types: `feat`, `fix`, `chore`, `refactor`, `docs`, `test`.

### Committing

- Commit early and often on your branch.
- Write clear commit messages in present tense:
  `Add project CRUD service and data-access layer`
- Stage only the files relevant to the commit. Never `git add -A` blindly.

### Pull Requests

- When a major change is complete, push the branch and create a PR.
- PR title: same format as branch name (`feat: project CRUD service`).
- PR description should reference the design doc(s) it implements.
- After pushing, use `gh pr create` to open the PR.
- Do not merge your own PR without review unless explicitly told to.

### Sync

- Before starting work, always `git pull origin main` to get the latest.
- Before pushing, `git fetch origin && git rebase origin/main` if the
  branch has been open for a while.

## Architecture Quick Reference

- **Control plane**: Go, single binary, k8s-style reconcilers
- **API**: Protobuf + Connect (gRPC + REST + streaming from one schema)
- **Database**: PostgreSQL 16 with RLS + transactional outbox
- **Event bus**: NATS JetStream
- **Telemetry**: OpenTelemetry → SigNoz (ClickHouse) — separated infra
- **Policy**: Rego (Open Policy Agent)
- **Runtime adapters**: gRPC sidecars (OpenCode first, CLI now / IPC later)
- **Frontend**: TypeScript + React + Vite + Connect-ES + React Flow
- **Object storage**: BlobStore abstraction (S3 + local filesystem)
- **Deployment**: Fully local (no cloud) is a supported mode

## Key Invariants (do not violate)

1. No business logic in the frontend — the UI reflects server state.
2. No hand-written API URLs — use the generated Connect-ES client.
3. No mutations outside the transactional outbox pattern.
4. No raw SQL outside the data-access layer.
5. Every `tenant_id` table must have an RLS policy (CI gate enforces).
6. Adapters never touch Postgres or NATS directly — gRPC stream only.
7. No automatic model failover — the human defines the exact model.
8. Recovery is opt-out, not opt-in.
9. Migrations are forward-only.

## Design Doc Index

| Doc | Subsystem |
|---|---|
| `docs/01_Architecture_Vision.md` | Tech direction, topology, principles |
| `docs/02_Domain_Model.md` | Entities, relationships, lifecycles |
| `docs/03_Scheduler_and_Runtime_Design.md` | Reconcilers, dispatch, health |
| `docs/04_Runtime_Adapter_SDK.md` | gRPC adapter contract, OpenCode |
| `docs/05_Worker_Specification.md` | Worker entity, permissions, budgets |
| `docs/06_Recovery_Workflow_Engine.md` | Triggers, recovery workflow, escalation |
| `docs/07_API_Specification.md` | Services, streaming, auth, webhooks |
| `docs/08_Event_Bus_and_Telemetry_Model.md` | NATS, OTel, SigNoz, events |
| `docs/09_Database_Schema.md` | Tables, outbox, RLS, migrations |
| `docs/10_Frontend_Architecture.md` | React, Connect-ES, workflow editor |
