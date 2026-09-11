# TUI ↔ GUI Parity Inventory

**Work item:** TUI Shell Overhaul (mockup parity) — `tui-shell-overhaul-mockup-parity-ask-default-with-collapsible-rails-tab-chrome-command-palette-real-screens-working-mouse-m72hs3ep84bexkvd`

Every GUI screen below is enumerated from the **real frontend route tree**
(`frontend/src/routeTree.gen.ts`) + **nav config** (`frontend/src/lib/nav-config.ts`), not from
memory. Each row maps the GUI screen → its TUI state (`exists` / `partial` / `missing`), the
tab that hosts it, and the **mutations** the surface implies (write paths = the child items that
carry GUI parity forward). Read-only `exists` rows are landed; `partial`/`missing` rows become
child work items under the **Terminal UI & File Diff Views** epic.

Legend — TUI state:
- **exists**: the data renderable now (read-only). May still lack write.
- **partial**: data + some detail render, but a mutation (create/edit/delete) or a sub-surface is absent.
- **missing**: no TUI surface (no source pane, no detail, no command).

The TUI is a first-class Orchicon client: keyboard-first with mouse support, Lipgloss theming,
the mockup's design language. It is **not** a literal port of GUI pixels, and it does not punt
whole areas to "use the web GUI".

## Ask Orchicon

| GUI route | Screen | TUI state | TUI tab | Notes / mutations |
|---|---|---|---|---|
| `/ask-orchicon`, `/conversations` | Ask Orchicon (chat + conversations) | **exists** | Ask | New chat (`/new`, lazy create on first send), composer, live transcript via the kit2 `Stream` widget (append preserves scroll offset; tail followed only at the bottom), rename (`/rename`), delete (`/delete`) with rail reconcile, ask-model picker (`/model` → `model_ref` at create), mode (`/mode` → `SetConversationMode`), attachments explicitly refused (`/attach`). |

## Overview

| GUI route | Screen | TUI state | TUI tab | Notes / mutations |
|---|---|---|---|---|
| `/dashboard` | Dashboard | **exists** | Overview | Aggregate plane state: executions + work items by status, worker/runtime-image health, recent activity (`ListExecutions` / `ListWorkItems` / `ListWorkers` / `ListRuntimeImages`) with a list + detail. |
| `/telemetry` | Telemetry | **exists** | Overview | Traces list + span detail (`TelemetryService.QueryTraces`) plus the live `StreamTelemetry` subscription (footer status, invalidate-on-event/reconnect). |
| `/cost-explorer` | Cost Explorer | **exists** | Overview | Usage/cost aggregation broken down by provider and by model with a grand total (`AIGatewayService.GetUsage` — the `orchicon_get_usage` usage-records shape). |

## Work

| GUI route | Screen | TUI state | TUI tab | Notes / mutations |
|---|---|---|---|---|
| `/projects` | Projects | **exists** (read-write) | Work | `Projects` source + detail. **Read-write:** `n` create (`CreateProject` with name/slug/goals/default runtime image) · `e` edit title/goals/project_dir (`UpdateProject`) · `d` set + create the project directory (`UpdateProject` + the `ListProjectFiles` probe that materializes/validates it). |
| `/projects/$id` | Project detail | **exists** | Work | Detail fields + goals + project dir + default image. |
| `/projects/new` | Create project | **exists** | Work | The `n` chord on Projects: the validated create form → `CreateProject`. |
| `/work-items` | Work Items | **exists** (read-write) | Work | `Work Items` source rendered through three DISPLAY groupings — **Tree** (real Epic→Feature→Task→Subtask DAG from `parent_id`), **Board** (grouped by real status, empty columns kept), **Archive** (`include_archived` — archived items + the status they restore to). `v` cycles; `T`/`B`/`Z` select. Kind badges + state pills render from real fields. **Read-write:** `n` create (`CreateWorkItem` with title/kind/parent/description/acceptance/priority/budgets JSON/context window/workflow/runtime image/context files/auto-start), `e` edit every mutable field (`UpdateWorkItem`), `s` status+priority, `t` schedule (`scheduled_start_at` + auto-start), `w`/`W` assign/unassign worker, `J`/`K` reorder children (`ReorderWorkItems` — the ONLY sequence mutation; display groupings never renumber), `a` archive / `R` restore / `x` delete → cancelled, each Confirm-gated. |
| `/work-items/$id` | Work Item detail | **exists** | Work | Detail fields (kind badge, state pill, parent, priority, budgets, context window, runtime image, worker, workflow, schedule, sort order, archived-from) + the description / acceptance-criteria body; diff pane opens for the work item's execution. |
| `/work-items/new` | Create work item | **exists** | Work | The `n` chord on Work Items: the typed create form (JSON budgets/context-window validation inline) → `CreateWorkItem`. |
| `/runtime-images` | Runtime Images | **exists** (read-write) | Work | `Runtime Images` source + detail (tag, status, base, version/built version, apt packages, toolchains, env, Dockerfile override). |
| `/runtime-images/$id` | Runtime Image detail | **exists** (read-write) | Work | Detail fields incl. apt packages/toolchains/env/Dockerfile override + the last build log. |
| `/runtime-images/new` | Create runtime image | **exists** | Work | The `n` chord on Runtime Images: the spec form (apt packages / toolchains / env as JSON, Dockerfile override) → `CreateRuntimeImage`; `e` edits the spec (`UpdateRuntimeImage`, version-carried), `b` builds with live streamed logs (`BuildRuntimeImage` → the kit2 `Stream` widget), `x` deletes (Confirm). |

## Execution

| GUI route | Screen | TUI state | TUI tab | Notes / mutations |
|---|---|---|---|---|
| `/workers` | Workers | **exists** | Control | Control list + detail. |
| `/workers/$id` | Worker detail | **exists** | Control | Detail fields + version list (`ListWorkerVersions`). **Mutations (child):** edit header (`UpdateWorker`), edit version draft (`UpdateWorkerVersion`), publish/deprecate (`PublishWorkerVersion`/`DeprecateWorker`), set active (`SetActiveWorkerVersion`). |
| `/workers/new` | Register worker | **missing** | — | **Child:** worker registration form (mutation). |
| `/workflows` | Workflows | **exists** | Automation | List + detail. |
| `/workflows/$id` | Workflow detail | **exists** | Automation | Detail + version trail. **Mutations (child):** `CreateWorkflow`, edit steps (`UpdateWorkflowVersion`), `PublishWorkflow`, `DeprecateWorkflow`, `CreateWorkflowVersion`. |
| `/workflows/new` | Create workflow | **missing** | — | **Child:** workflow DSL/definition form (mutation). |
| `/workflows/$id/runs/$runId` | Workflow run detail | **exists** | Execution (runs) | Detail + step runs body (status per step) + failure diagnosis for a failed run (failed/blocked steps and the linked failed executions' error messages); live Execution detail pane. **Mutations:** `RetryFailedWorkflowRun` (Confirm) · `ForceProgressWorkflowRun` (Confirm) |
| `/executions` | Executions | **exists** | Execution | Live list + detail. **Mutations:** `CancelExecution` (Confirm, reason recorded) · `SendExecutionMessage` (interject form) — both reconcile the list. |
| `/executions/$id` | Execution detail | **exists** | Execution | Live session view (Stream widget, scroll preserved) + diff pane. **Mutations:** `CancelExecution` · `SendExecutionMessage`. |
| `/schedules` | Schedules | **exists** | Automation | Same surface as `/recurring-items` (recurring work items); create/edit/pause/delete landed. |

## Automation

| GUI route | Screen | TUI state | TUI tab | Notes / mutations |
|---|---|---|---|---|
| `/recurring-items` | Recurring Items | **exists** | Automation | `Recurring Items` source (`ListWorkItems` + `RecurringFilter_ONLY_RECURRING`); mutations: `n` create, `e` edit, `p` pause/resume, `x` delete (confirm) via `CreateWorkItem` / `UpdateWorkItem` (recurring_schedule + recurring_enabled) / `DeleteWorkItem`. |
| `/recurring-items/$id` | Recurring item detail | **exists** | Automation | Detail (cadence + next fire) + per-fire run history: fire status, bound workflow run, that run's executions/outputs (`GetWorkItemRunHistory`). |
| `/recurring-items/new` | Create recurring item | **exists** | Automation | Validated create form (project, kind, workflow binding, frequency/interval/days/start date+time, outputs mode) with the next fire computed server-side; `ctrl+s` saves, `esc` cancels. |
| `/idea-cloud` | Idea Cloud | **exists** | Automation | `Idea Cloud` source (`ListIdeas`, idea_state_scope ACTIVE) with provenance (`spawned_by` + `spawned_by_run_id` + spawned-by-title badge); `p` promote (`PromoteIdea`), `x`+`y` dismiss (`DismissIdea`, confirm). A separate `Rejected Ideas` pane renders idea_state_scope REJECTED — the durable rejection history the automation dedupe gate consults. |

## Enforcement

| GUI route | Screen | TUI state | TUI tab | Notes / mutations |
|---|---|---|---|---|
| `/approvals` | Pending Approvals | **exists** | Enforcement | List + detail — the detail shows the upstream summary, acceptance criteria, touched files, the rejection reason and the recorded policy decision (`require_approval`, which policy). **Read-write:** `a` approve · `x` reject (`ApproveStep`), form-gated by the reason prompt, disabled while in flight, list reconciles after the write. |
| `/policies` | Policies | **exists** | Enforcement | List + detail. **Read-write:** `n` create (`CreatePolicy`) · `e` edit the draft version (`UpdatePolicyVersion`) · `p` publish a version (`PublishPolicy`, Confirm) · `v` list versions. |
| `/policies/$id` | Policy detail | **exists** | Enforcement | Detail + version trail; `v` opens the version picker and `enter` inspects a version's Rego/OPA body **verbatim** (no silent truncation). |
| `/policies/new` | Create policy | **exists** | Enforcement | The `n` chord on Policies: the definition form (name / decision point / scope / effect / query / version note / Rego module) → `CreatePolicy`, the module body sent whole. |
| `/recovery` | Recovery | **exists** | Enforcement | Recoveries source (`ListRecoveries`) + recovery-events stream. **Read-write:** `a` approve continuation plan · `x` reject plan (reason required) · `c` cancel recovery · `m` mark task succeeded — all Confirm-gated. |
| `/recovery/$id` | Recovery detail | **exists** | Enforcement | Recovery detail + the continuation plan, plus the **available action surface** the plane allows for that recovery/plan state. |

## Control

| GUI route | Screen | TUI state | TUI tab | Notes / mutations |
|---|---|---|---|---|
| `/webhooks` | Webhooks | **exists** (read-write) | Control | `Webhooks` source + detail (`ListSubscriptions`). Create/edit/delete subscription through the form + Confirm-gated actions (`CreateSubscription` / `UpdateSubscription` / `DeleteSubscription`), `TestSubscription`, and the deliveries log (`ListDeliveries`) rendered in the detail body. |
| `/adapters` | Adapters | **exists** (read + local toggle) | Control | `Adapters` source + detail (`ListAdapters` + the capability manifest). Enable/disable is a client-side dispatch filter: the public `RuntimeAdapterService` is read-only (adapters self-register over the sidecar gRPC contract), so there is no adapter write RPC to call. |
| `/settings` | Settings | **exists** (read-write) | Control | `Settings` source + detail (every field: models, stall knobs, reaper, budgets, backup/log, session TTLs). `e` opens the typed form and saves through `UpdateSettings`; model refs are validated inline (provider/model) before submit. |
| `/admin` | Admin | **exists** (admin-gated) | Control | `Admin` source: the admin surface inventory plus an EXPLICIT live permission state (probed via an admin-gated read) — a credential without the admin scope sees "permission required", never a silent empty pane. |
| `/usage` | Usage | **exists** | Overview (Usage Records) | Raw usage-records table + per-record detail (`AIGatewayService.GetUsage`). |

## This run closed (real screens replacing GUI-mirror stubs)

The following notice-only “use the web GUI” commands were **removed** (QA finding 4) and
replaced by real TUI surfaces on the Control screen:

- **Providers** → `Providers` source + detail (`ProviderService.ListProviders`).
- **Webhooks** → `Webhooks` source + detail (`WebhookService.ListSubscriptions`).
- **Settings** → `Settings` source + detail (`SettingsService.GetSettings`).
- **Runtime Images / Secrets / MCP / Workers** — already real Control sources.

## Control write parity (this run)

The Control screen is now read-WRITE for every surface the GUI mutates:

- **Settings** — view all + edit/save (`UpdateSettings`), model refs validated inline.
- **Webhooks** — create/edit/delete subscription + test + deliveries view.
- **Adapters** — `RuntimeAdapterService` client wired, source + detail + enable/disable.
- **MCP servers** — create/edit/delete, enabled toggle, credential store/clear via the secret
  store, and install (where the runtime supports it).
- **Providers** — create custom/edit/delete, enable/disable, token store/clear.
- **Secrets** — names/metadata only + create/update/delete by name; no code path reads a value
  (`GetSecret` is never called).
- **Admin** — reachable with an explicit permission state.

Nine sources render ONE pane at a time (focused source + detail) — a nine-across grid truncates
beyond reading. Secrets/provider tokens/MCP credentials are `${SECRET_NAME}` references: values
are written once through the form (masked) and never fetched or rendered.

The `/providers`, `/webhooks`, `/settings` slash commands resolve to these real panes (no
notice-only list remains in the registry — asserted by `TestNoNoticeOnlyCommands`).

## Work write parity (this run)

The Work tab is now read-WRITE for every Work surface the GUI mutates:

- **Work Items** — create from a typed `Form` (`CreateWorkItem`), edit every mutable field
  (`UpdateWorkItem`), change status/priority, schedule, assign/unassign a worker, reorder children
  (`ReorderWorkItems`), archive/restore and delete (→ cancelled). Destructive actions are
  Confirm-gated and the list reconciles after the write.
- **Tree / Board / Archive** — display groupings over the real fields: the Tree walks the actual
  `parent_id` DAG (max 4 levels), the Board groups by status (a display sort never touches
  `sort_order` — only `ReorderWorkItems` does), the Archive reads `include_archived`.
- **Projects** — create (`CreateProject`), edit title/goals/project_dir (`UpdateProject`), and
  set/create the project directory. `ProjectService` has no `CreateProjectDirectory` RPC (its 10
  RPCs are Create/Get/List/Update/Archive/Delete/Pause/Activate/StreamProjectEvents/
  ListProjectFiles), so the directory is set through `UpdateProject` and then probed with
  `ListProjectFiles` — a bad path fails immediately instead of at worker dispatch.
- **Runtime Images** — create/edit the spec and **build** it: `BuildRuntimeImage` is a
  server-stream whose log chunks render live through the kit2 `Stream` widget (append preserves
  the operator's scroll offset), and the build status transition (draft → building → ready|failed)
  is reflected in the row pill and the detail pane. Delete is Confirm-gated.

Budgets / context-window / apt-packages / toolchains / env are JSON — the `Form`'s `json` field
type validates them before submit.

## Recomputed child work items

Every `missing` row above is a child work item under the **Terminal UI & File Diff Views** epic.
Each child carries this grounding: the GUI route (from `routeTree.gen.ts`), the RPCs it consumes
(from the matching `apiv1connect` client), and the mutation it implies. High-priority children:

1. **Work-item create/edit + Tree/Board/Archive toggles** (`/work-items/new`, `/work-items/$id`) — mutates `WorkItemService`.
2. **Create/edit project** (`/projects/new`) — mutates `ProjectService`.
3. **Approve/reject step approval** (`/approvals`) — mutates `ApprovalService`. **Landed:** the Enforcement screen's `a`/`x` chords (`ApproveStep`) with the reason form, in-flight guard, and list reconciliation.
4. **Recovery actions** (`/recovery`, `/recovery/$id`) — mutates `RecoveryService`. **Landed:** `a`/`x`/`c`/`m` on the Recoveries source (`ApproveContinuationPlan`, `RejectContinuationPlan`, `CancelRecovery`, `MarkTaskSucceeded`), all Confirm-gated.
5. **Policies create/edit/publish + version inspection** (`/policies`, `/policies/new`) — mutates `PolicyService`. **Landed:** `n`/`e`/`p`/`v` on the Policies source (`CreatePolicy`, `UpdatePolicyVersion`, `PublishPolicy`, `ListPolicyVersions`); the `/policies/new` row moves missing → exists.
6. ~~**Schedules + recurring-item create/edit** (`/schedules`, `/recurring-items/new`) — mutates `WorkItemService`~~ — **landed** (Automation: Recurring Items create/edit/pause/resume/delete + per-fire run history).
7. **Webhook subscription create/edit/delete + deliveries** (`/webhooks`) — mutates `WebhookService`.
8. **Settings edit/save** (`/settings`) — mutates `SettingsService`.
9. **Adapters list/enable** (`/adapters`) — wire `AdapterService` client + source.
10. **Dashboard** (`/dashboard`) — landed: the Overview tab's Dashboard source.
11. **Cost Explorer / Usage** (`/cost-explorer`, `/usage`) — landed: the Overview tab's Cost Explorer + Usage Records sources.
12. ~~**Idea Cloud** (`/idea-cloud`) — idea triage list~~ — **landed** (Automation: idea list with provenance, rejected section, promote/dismiss).
13. **Admin** (`/admin`) — admin-gated surfaces.
