# ADR-0001: Fix AskOrchicon create_worker / create_workflow ghost records

**Status:** Proposed (step 1 — Principal Software Architect)
**Work item:** Fix: AskOrchicon create_worker / create_workflow tools create ghost records

## Context

The AskOrchicon/MCP tool path (`internal/askorchicon/tool_workers.go`, `tool_workflows.go`)
inserts **only** the header row (`workers` / `workflows`) and commits. It never creates the
mandatory draft version-1 row, drops `model_ref`/`runtime_ref` (worker) and `steps` +
`description` (workflow), bypasses the outbox event, and writes no `worker.created` /
`workflow.created` audit row. Result: ghost records that the UI cannot edit or publish
(Edit/Publish gate on the existence of a version row). The proper service paths
(`internal/worker/service.go CreateWorker`, `internal/workflow/service.go CreateWorkflow`)
do all of the above in one transaction.

Root cause is architectural: **two independent implementations of "create a worker/workflow"**
have drifted. Patching the tool path with a third copy of the create logic would reproduce
the same drift. Established facts (verified in this worktree):

- `internal/worker` and `internal/workflow` do NOT import `internal/askorchicon` → no import
  cycle if askorchicon delegates to them.
- The outbox/audit/slug/prompt helpers (`enqueueWorkerEvent`, `buildEventPayload`,
  `recordAudit`, `uniqueSlug`, `composeWorkerPrompt`, `workerAuditSnapshot`,
  `workflowVersionAuditSnapshot`, `validateName`, `validateStepsField`, `validateJSONField`)
  are unexported inside the worker/workflow packages — the shared core must live THERE.
- `WorkflowRow` has **no description column** (and no `description` in
  `CreateWorkflowRequest`); the tool's `description` param was silently dropped.
- The worker service's `UpdateWorker` writes `worker.updated` audit but no outbox event —
  the tool must match that (audit only).
- The askorchicon package has its own `recordAudit` helper (service.go:562) for in-package
  audit writes.
- DB-backed tests in this repo are opt-in via `ORCHICON_TEST_DSN` (e.g.
  `postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable`) — see
  `internal/worker/bulk_update_worker_model_test.go` for the pattern.

## Decision

**D1 — Single shared create core; both the Connect handlers and the tool paths delegate.**
Extract the transactional create (validation, slug dedupe, header + version-1 inserts,
outbox event, audit) into exported functions in the domain packages.

`internal/worker/create.go` (new):

```go
type CreateWorkerInput struct {
    TenantID, Name, Slug, Description, Purpose, VersionNote string
    RuntimeRef, ModelRef                                    string
    Role, Skills, Behavior, AgentsMD, SystemPrompt          string
}

// ValidateCreateWorkerInput trims/bounds-checks every field and derives the
// slug from Name when Slug == "" (normalizeSlug semantics). Returns error on
// invalid input; callers map it to CodeInvalidArgument (handler) or a plain
// error (tool).
func ValidateCreateWorkerInput(in *CreateWorkerInput) error

// CreateWorkerTx expects validated input. Inside the caller's tenant tx it:
// dedupes the slug (uniqueSlug: -2, -3…), inserts the worker header (draft,
// CurrentVersion 0), inserts draft version-1 (refs + JSON defaults "[]"/"{}"
// for context_sources/permissions/gated_tools/budget_overrides/labels),
// composes system_prompt (composeWorkerPrompt when any structured field is
// set, else raw SystemPrompt), enqueues worker.created (outbox), and writes
// the worker.created audit row with the service's exact snapshot
// {id,name,slug,status,version,model_ref}. Does NOT commit.
func CreateWorkerTx(ctx context.Context, tx pgx.Tx, in CreateWorkerInput) (db.WorkerRow, db.WorkerVersionRow, error)
```

`internal/workflow/create.go` (new):

```go
type CreateWorkflowInput struct {
    TenantID, ProjectID, Name string
    Type        string // "" | one_shot | template; "" derives (template if no project, else one_shot)
    GitStrategy string // "" | local | pr | none
    VersionNote string
    Steps       string // JSON array string; "" → "[]" (validateStepsField semantics)
    Inputs      string // "" → "{}"
    Outputs     string // "" → "{}"
}

func ValidateCreateWorkflowInput(in *CreateWorkflowInput) error
// CreateWorkflowTx: RequireProjectActive when ProjectID != "", inserts header
// (draft, CurrentVersion 0) + draft version-1 (steps/inputs/outputs), enqueues
// workflow.created (outbox), writes workflow.created audit with
// workflowVersionAuditSnapshot(createdVersion). Does NOT commit.
func CreateWorkflowTx(ctx context.Context, tx pgx.Tx, in CreateWorkflowInput) (db.WorkflowRow, db.WorkflowVersionRow, error)
```

Refactor `Service.CreateWorker` / `Service.CreateWorkflow` to: build input from proto →
Validate (→ `CodeInvalidArgument`) → BeginTenantTx → CreateXxxTx (→ `mapDBError` /
`CodeInternal`) → Commit → proto mapping. **Connect error codes and observable behavior are
preserved**; validation stays outside the tx exactly as today. `make ci` + existing service
tests gate the refactor.

**D2 — Tool paths become thin adapters.**

`toolCreateWorker`: params = existing name/purpose/model_ref/runtime_ref **plus** optional
`description`, `version_note`, `role`, `skills`, `behavior`, `agents_md`, `system_prompt`.
Validate → BeginTenantTx → `worker.CreateWorkerTx` → commit. Response: the flat worker-row
JSON as today **plus** `version` (int, =1) and `version_id` keys — backward compatible with
existing callers that read `id`. `makeWorkerSlug` usage is replaced by the core's
normalize+dedupe (delete the helper if it has no other callers).

`toolCreateWorkflow`: params = `name` (required) **plus** optional `steps` (JSON array —
declare as `array` in the tool schema; unmarshal into `json.RawMessage`, pass
`string(raw)` to the core), `description`, `version_note`, `type`, `git_strategy`,
`inputs`, `outputs` (JSON-object strings), `project_id` (optional). Response: flat
workflow-row JSON + `version`/`version_id`.

**D3 — Workflow `description` maps to v1 `version_note` (fallback).** There is no workflow
description column; a column migration is out of proportion for this fix. The tool accepts
`description` and stores it as the version-1 `version_note` when `version_note` is empty
(description → version_note fallback, in the tool adapter). Document this in the schema
description. A proper `workflows.description` column is future work (recorded, not built).

**D4 — `toolUpdateWorker` aligns to service semantics without extraction.** Keep header-only
edits (name, purpose — matches `Service.UpdateWorker`), add optional `description`
(`db.UpdateWorkerFields.Description`), and write a `worker.updated` audit row via the
package's `recordAudit` with before/after header snapshots {id,name,description,purpose,status}.
No outbox event — the service writes none for updates; this IS parity.

**D5 — Tool schema text updated in `internal/askorchicon/tools.go`.** `create_worker` /
`create_workflow` descriptions and Properties reflect the new params (the current
create_workflow text says "optional template" — a lie; `steps` was silently dropped). This
keeps the Ask Orchicon tool registry in sync with the platform (AGENTS.md invariant).

**D6 — Out of scope (recorded, not built):** a UI "no versions — create one" affordance for
zero-version records. Primary fix makes that state unreachable via tool paths; the UI change
is a separate frontend task. Also out of scope: data repair of the ghosted workflow
`01M13DYM9525V1RA7Q5PPV41CM` (task states code fix only).

## Consequences

- Ghost records become structurally impossible through either path — one implementation,
  no drift; audit trail and event feed gain `worker.created` / `workflow.created` for
  tool-created records (actor = the Ask Orchicon caller from ctx).
- Created workers/workflows are immediately editable + publishable in the UI (draft v1
  exists; Edit/Publish gates render).
- Slightly larger refactor surface: the two Connect handlers change shape but not behavior;
  existing worker/workflow service tests protect against regression. Connect error-code
  mapping must be reviewed in PR (validation → InvalidArgument; tx errors → mapDBError).
- Tool args previously ignored (`description` on workflow) now persist (as version_note) —
  a behavior change, but the honest one; silently dropping was the bug.
- `create_workflow` gains the ability to seed steps, unblocking "create a workflow from a
  template" flows from Ask Orchicon.

## Files touched (delta for the implementer)

| File | Change |
|---|---|
| `internal/worker/create.go` | NEW — CreateWorkerInput, ValidateCreateWorkerInput, CreateWorkerTx |
| `internal/workflow/create.go` | NEW — CreateWorkflowInput, ValidateCreateWorkflowInput, CreateWorkflowTx |
| `internal/worker/service.go` | CreateWorker refactors to delegate to the core (behavior-preserving) |
| `internal/workflow/service.go` | CreateWorkflow refactors to delegate to the core (behavior-preserving) |
| `internal/askorchicon/tool_workers.go` | toolCreateWorker delegates to core; toolUpdateWorker +description +audit |
| `internal/askorchicon/tool_workflows.go` | toolCreateWorkflow delegates to core, full param set |
| `internal/askorchicon/tools.go` | schema/properties/description updates for both tools |
| `internal/askorchicon/tool_workers_test.go` | NEW — DB-backed tests (ORCHICON_TEST_DSN pattern) |
| `internal/askorchicon/tool_workflows_test.go` | NEW — DB-backed tests |

## Test plan (acceptance mapping)

DB-backed tests skip unless `ORCHICON_TEST_DSN` is set (repo pattern); `make ci` runs what
runs in CI. Tests must assert, via the tool Fns directly (`toolCreateWorker(ctx, pool, args)`):

1. create_worker → `db.GetLatestWorkerVersion` returns draft v1; `model_ref`/`runtime_ref`
   persisted on the version; `workers.current_version` still 0.
2. create_worker with role/skills → version `system_prompt` = composed form.
3. create_workflow with steps → `db.GetWorkflowVersion(wf.ID, 1)` returns the steps JSON
   array persisted; draft status.
4. Audit rows exist (`worker.created`, `workflow.created`) for tool-created records.
5. Publishability: `db.PublishWorkerVersion` / `db.PublishWorkflowVersion` on the created
   draft v1 succeeds (proves Edit/Publish render).
6. Second create with the same name gets a deduped slug (-2), no constraint error.
7. Validation failures: empty name, steps not an array, bad git_strategy/type.
