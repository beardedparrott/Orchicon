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
| `/projects` | Projects | **exists** | Work | List + detail (read-only). **Child:** project create/edit, repo link. |
| `/projects/$id` | Project detail | **exists** | Work | Detail fields + goals. |
| `/projects/new` | Create project | **missing** | — | **Child:** project creation form (mutation). |
| `/work-items` | Work Items | **exists** | Work | List + detail (read-only). **Child:** work-item create/edit/status mutation, kind badges, state pills, Tree/Board/Archive toggles (mockup). |
| `/work-items/$id` | Work Item detail | **exists** | Work | Detail fields + acceptance criteria; diff pane opens for the work item's execution. |
| `/work-items/new` | Create work item | **missing** | — | **Child:** work-item creation form (mutation). |
| `/runtime-images` | Runtime Images | **exists** | Control | Control list + detail. |
| `/runtime-images/$id` | Runtime Image detail | **exists** | Control | Detail fields incl. apt packages/toolchains. |
| `/runtime-images/new` | Create runtime image | **missing** | — | **Child:** image build definition form (mutation). |

## Execution

| GUI route | Screen | TUI state | TUI tab | Notes / mutations |
|---|---|---|---|---|
| `/workers` | Workers | **exists** | Control | Control list + detail. |
| `/workers/$id` | Worker detail | **exists** | Control | Detail fields. |
| `/workers/new` | Register worker | **missing** | — | **Child:** worker registration form (mutation). |
| `/workflows` | Workflows | **exists** | Automation | List + detail. |
| `/workflows/$id` | Workflow detail | **exists** | Automation | Detail + version trail. |
| `/workflows/new` | Create workflow | **missing** | — | **Child:** workflow DSL/definition form (mutation). |
| `/workflows/$id/runs/$runId` | Workflow run detail | **exists** | Execution (runs) | Detail + step runs body; live Execution detail pane. |
| `/executions` | Executions | **exists** | Execution | Live list + detail, interjection message send (mutation: `SendExecutionMessage`). |
| `/executions/$id` | Execution detail | **exists** | Execution | Live session view + diff pane. |
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
| `/webhooks` | Webhooks | **exists** | Control (this run) | New `Webhooks` source + detail (`ListSubscriptions`). **Child:** create/edit/delete subscription (mutation), deliveries/replay. |
| `/adapters` | Adapters | **missing** | — | No TUI source; the Adapter client is not yet wired. **Child:** adapter list + detail + enable/disable (mutation). |
| `/settings` | Settings | **exists** | Control (this run) | New `Settings` source + detail (`GetSettings`). **Child:** settings edit/save (mutation). |
| `/admin` | Admin | **missing** | — | Admin-gated; **child:** admin surfaces. |
| `/usage` | Usage | **exists** | Overview (Usage Records) | Raw usage-records table + per-record detail (`AIGatewayService.GetUsage`). |

## This run closed (real screens replacing GUI-mirror stubs)

The following notice-only “use the web GUI” commands were **removed** (QA finding 4) and
replaced by real TUI surfaces on the Control screen:

- **Providers** → `Providers` source + detail (`ProviderService.ListProviders`).
- **Webhooks** → `Webhooks` source + detail (`WebhookService.ListSubscriptions`).
- **Settings** → `Settings` source + detail (`SettingsService.GetSettings`).
- **Runtime Images / Secrets / MCP / Workers** — already real Control sources.

The `/providers`, `/webhooks`, `/settings` slash commands resolve to these real panes (no
notice-only list remains in the registry — asserted by `TestNoNoticeOnlyCommands`).

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
