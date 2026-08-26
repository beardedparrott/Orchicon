# Architecture Notes — 4 Split Cost Explorer from Telemetry — Dedicated Overview Sections

**Work item:** 4 — Split Cost Explorer from Telemetry — Dedicated Overview Sections
**Branch:** `4-split-cost-explorer-from-telemetry-dedicated-overview-sections-c1t9ea4yqza03jck`
**Author (step 1):** Principal Software Architect
**Date:** 2026-08-26
**Status:** Decision — ready for Design Approver → Engineer

## 1. Context

`frontend/src/routes/telemetry.tsx` is 42 KiB and hosts 6 local tabs (`overview| cost| credits| traces| metrics| logs`) — the OTEL-native telemetry UI (traces/logs/metrics via `useQueryTraces / useQueryLogs / useQueryMetrics` + `GrafanaEmbed` iframe at `GRAFANA_UI_URL=/grafana`) is intertwined with the full Cost Explorer (`CostExplorer`, `WorkflowCostPanel`, `UsageRecordsTable` plus `components/cost-explorer/{SearchInput,SortControls,SortableHeader,utils}` with fresh-vs-cache split `fmtSplitHit(cacheRead, cacheWrite, prompt)` and workflow drill-down). Task 2 already scaffolded the **Overview** dropdown in `components/app-shell.tsx` as three first-class entries — Dashboard `/dashboard` (`LayoutDashboard` cyan), Telemetry `/telemetry` (`Activity` emerald), Cost Explorer `/cost-explorer` (`Coins` amber) — and `routeTree.gen.ts` already contains `/cost-explorer`, but `routes/cost-explorer.tsx` is a 2.6 KiB placeholder (two static tiles, copy says "For full breakdown, visit Telemetry" — the inverse of the desired split). `routes/dashboard.tsx` cost tiles have no deep-links. Both data planes already share the same backend layer (`internal/api` + `usage`: `api/aigateway.ts` `useGetCost/useGetUsage/useGetWorkflowCosts` against `AIGatewayService` Postgres source-of-truth, and `api/telemetry.ts` `useGetDashboard/useQueryTraces|Metrics|Logs` against `TelemetryService` Tempo/Loki/VictoriaMetrics), but the frontend renders them intertwined.

Goals per acceptance: Telemetry stops embedding Cost Explorer and vice versa, each gets its own distinct URL under Overview with correct Lucide icons, both fetch from shared cost/usage data but render independently (traces/logs vs token/cost breakdown), no data regression (cache-vs-fresh split visible), dashboard + breadcrumb links route correctly, functional parity preserved (category folders, filters/sorts/pagination/CRUD/bulk/YAML/deep-links/drag-drop/search identical — only split + Recurring Items type are intentional), both pages uplifted to Token 1 glass styling, and the `mockup2.html` conversation panel remains Ask Orchicon ONLY (no persistent history panel leaked to these routes — scope guardrail).

## 2. Decision (ADR-004)

**Title:** Split telemetry.tsx into two route-owned pages sharing the cost/usage data layer; transplant cost-explorer components verbatim; telemetry becomes pure OTEL-native.

### 2.1 Alternatives Considered

| Option | Description | Pros | Cons | Verdict |
|---|---|---|---|---|
| **A — Full split (chosen)** | `telemetry.tsx` keeps `OverviewPanel(dashboard telemetry widgets) + TracesPanel + MetricsPanel + LogsPanel + GrafanaEmbed + CreditsPanel`; `cost-explorer.tsx` owns `CostExplorer + WorkflowCostPanel + UsageRecordsTable` plus `SearchInput/SortControls/SortableHeader/utils`. No tab state. Distinct URLs `/telemetry` and `/cost-explorer`. | Distinct URLs per AC, zero coupling, each route owns its query keys, trivial code-ownership, matches mockup 3-entry Overview, eliminates 42 KiB indirection. | One-time move + imports; need Dashboard link fix. | **Chosen** |
| B — Keep single file, route param `?view=` | Single component renders both; router aliases `/telemetry` and `/cost-explorer` to same file with param. | Minimal diff | Violates "Telemetry no longer embeds Cost Explorer" AC; URL not distinct; tab state leaks; future OTEL overhaul re-entangles costs. | Rejected |
| C — Nested layout `routes/overview/{telemetry,cost-explorer}.tsx` | Hierarchical URLs `/overview/telemetry` | URL encodes hierarchy | Breaks flat canonical contract from ADR-003, fractures deep-links/bookmarks, requires `routeTree.gen.ts` regeneration and redirects, rejected by Task 3. | Rejected |

### 2.2 Chosen Design (Option A)

1. **Keep flat canonical URLs** — Do not hand-edit `routeTree.gen.ts`. `/telemetry` and `/cost-explorer` remain flat children of `__root__` as today. ADR-003 contract preserved.
2. **Telemetry page (`routes/telemetry.tsx` → ~14 KiB):**
   - Remove imports `useGetCost/useGetUsage/useGetWorkflowCosts`, `useListProjects`, `workItemClient`, `SearchInput/SortControls/SortableHeader`, `filter*/sort*` utils, and `RollupMode`/`UsageRollupEnum`. Keep `useGetDashboard/useQueryTraces|Metrics|Logs/useListProviders`, `GrafanaEmbed`, `StatCard`, `OverviewPanel` (total tokens/cost/executions/providers + cost-by-model quick glance — the `dashboard telemetry widgets`), `TracesPanel`, `MetricsPanel`, `LogsPanel`, `CreditsPanel`.
   - Replace tab bar with direct native UI: if retaining tabs, keep only `traces|metrics|logs|overview|credits` — drop `cost`. Preferred: no cost tab at all; default to `overview`. Breadcrumbs: `Overview > Telemetry`.
   - OTEL-native replacement for iframed Grafana is progressive: keep `GrafanaEmbed` as fallback but add native list sections (`Recent traces/logs` + degraded polling already exists). Future `Telemetry View Overhaul` swaps iframe for native OTEL panels without touching cost code.
   - Glass: `Card` already is `glass-panel rounded-2xl`; page wrapper uses `bg-mesh` via `AppShell` (no extra work). Ensure `StatCard` tiles are `glass-panel` not `bg-card` where appropriate.
3. **Cost Explorer page (`routes/cost-explorer.tsx` → ~22 KiB):**
   - Transplant verbatim from `telemetry.tsx`: `CostExplorer` (rollup `PROJECT|TASK|EXECUTION|MODEL|workflow`, scope `projectId/taskId/executionId`, drill-down `handleRowClick/clearScope/rollbackOneLevel`, `displayName` via `projectNameMap/taskNameMap`), `WorkflowCostPanel` (expand workflow/run, `filterWorkflowAggregates/sortWorkflowAggregates`), `UsageRecordsTable` (search+sort `filterUsageRecords/sortUsageRecords`), helpers `fmtSplitHit`, `fmtInt`, `fmtWhen`, `toNum`, `statusBadge`.
   - Keep shared data layer: `useGetCost/useGetUsage/useGetWorkflowCosts/useListProjects` + `workItemClient` for task titles. Ensure fresh-vs-cache split visible on every row and window total: `fmtSplitHit(cacheReadTokens, cacheWriteTokens, promptTokens)` already covers `DB migration: cache/reasoning token columns` — must not regress. Provider/model breakdown via `By Model` rollup and `WorkflowCostPanel` `provider/model` columns. Project/work-item attribution via `By Project / By Task` + `projectNameMap/taskNameMap`.
   - Preserve local search/sort behavior: filtering/sorting runs over already-fetched page (no server round-trip) via `components/cost-explorer/utils.ts` — reuse without change. `SearchInput/SortControls/SortableHeader` stay.
   - Glass styling: wrap drill-down + workflow + records sections in `Card glass-panel`; rollup pills use `glass-menu` tokens where applicable. Full-width per-page content (no ask-orchicon canvas split).
   - Breadcrumbs: `Overview > Cost Explorer` with back/clear scope affordances; deep-links preserve `projectId/taskId/executionId` search params.
4. **Shared layer unchanged:** `api/aigateway.ts` (`usageKeys.cost`, `useGetCost`, `useGetUsage`, `useGetWorkflowCosts`) and `api/telemetry.ts` (`telemetryKeys`, `useGetDashboard`) stay shared; both pages fetch independently but from same Postgres usage_records source. No backend change.
5. **Dashboard links (`routes/dashboard.tsx`):** Make cost tiles link to correct split page: `Total Spend` + `Total Tokens` tiles → `<Link to="/cost-explorer">` ; telemetry-related tiles if any → `/telemetry`. Tiles keep `glass-panel` uplift (replace `bg-card` if needed). This satisfies "Dashboard links point to correct split page."
6. **Navigation (`components/app-shell.tsx`):** Already correct per mockup (Overview: Dashboard/LayoutDashboard cyan, Telemetry/Activity emerald, Cost Explorer/Coins amber). No change except verify active-state: dropdown item active when `path === to || path.startsWith(to + "/")` (share helper from ADR-003). Mobile drawer mirrors.
7. **Scope guardrail:** Do not add `ConversationPanel` or history to either route. `AppShell` already gates it via `isAskOrchicon = path === "/ask-orchicon" || path.startsWith("/ask-orchicon/")` — leave untouched. Shell body on these routes is per-page full-width.
8. **No functional regression:** Category folders, filters/sorts/pagination/CRUD/bulk/YAML/deep-links/drag-drop/search live outside these two routes — untouched. Cost Explorer search/sort keeps same `utils.ts` semantics (including tie-breakers, `finished` null-last).

## 3. Consequences

- **Positive:** Distinct URLs per AC; telemetry and cost concerns decoupled; each page can evolve (OTEL-native vs cost attribution) without merge conflicts; shared data layer prevents divergence; fresh-vs-cache visibility preserved; dashboard deep-links correct; glass tokens consistent.
- **Negative:** One-time 20 KiB+ move; `telemetry.tsx` git history split (retain `git log --follow`).
- **Risks & mitigations:** Stale import after move → `tsc --noEmit` + `npm run build` in CI. Cost placeholder copy inversion ("visit Telemetry") → replace with "View traces in Telemetry". Missing breadcrumb params → pass `projectId` via `search` not path.

## 4. File & Function Map (for Engineer)

| Area | File | Change |
|---|---|---|
| Telemetry (pure OTEL) | `frontend/src/routes/telemetry.tsx` | Delete `CostExplorer/WorkflowCostPanel/UsageRecordsTable` and cost imports; keep `OverviewPanel/TracesPanel/MetricsPanel/LogsPanel/CreditsPanel/GrafanaEmbed/StatCard`; drop `cost` tab; add breadcrumbs `Overview > Telemetry`; ensure `glass-panel` cards |
| Cost Explorer (standalone) | `frontend/src/routes/cost-explorer.tsx` | Replace placeholder with transplanted `CostExplorer + WorkflowCostPanel + UsageRecordsTable + fmtSplitHit/fmtInt/fmtWhen/toNum/statusBadge`; imports `api/aigateway`, `api/projects`, `workItemClient`, `components/cost-explorer/*`; wrap in `Card glass-panel`; breadcrumbs `Overview > Cost Explorer` |
| Shared utils | `frontend/src/components/cost-explorer/utils.ts` + `SearchInput.tsx` + `SortControls.tsx` + `SortableHeader.tsx` | **No logic change** — reused verbatim by Cost Explorer; telemetry stops importing |
| Shared data | `frontend/src/api/aigateway.ts`, `frontend/src/api/telemetry.ts`, `frontend/src/api/clients.ts` | No change — both pages consume same clients |
| Dashboard links | `frontend/src/routes/dashboard.tsx` | Wrap `Total Spend/Total Tokens` Tiles in `<Link to="/cost-explorer">`; optional telemetry tiles → `/telemetry`; uplift Tiles to `glass-panel` if still `bg-card` |
| Navigation | `frontend/src/components/app-shell.tsx`, `frontend/src/routeTree.gen.ts` | Verify Overview entries (Dashboard `LayoutDashboard` cyan, Telemetry `Activity` emerald, Cost Explorer `Coins` amber) already correct; **do not hand-edit** `routeTree.gen.ts` |
| Breadcrumbs | `frontend/src/components/app-shell.tsx` or new `components/breadcrumbs.tsx` per ADR-003 | Render `Overview > Telemetry` and `Overview > Cost Explorer` outside ask-orchicon guard |

## 5. Acceptance Mapping

- [ ] Telemetry no longer embeds Cost Explorer; Cost Explorer has its own route under Overview with distinct URL — verify `/telemetry` has no cost drill-down, `/cost-explorer` does, each is `Overview` dropdown entry.
- [ ] Both pages fetch from shared cost/usage data but render independently — `telemetry` uses `telemetryKeys`, `cost-explorer` uses `usageKeys.cost/records/workflow-costs` on same `usage_records` source.
- [ ] Overview dropdown shows Dashboard / Telemetry / Cost Explorer as three separate entries with correct Lucide icons — per `NAV_GROUPS` + mockup.
- [ ] Direct links from Dashboard + telemetry breadcrumbs navigate to correct split page — Dashboard cost tiles → `/cost-explorer`, breadcrumb `Overview > Telemetry/Cost Explorer`.
- [ ] No data regression — `fmtSplitHit(cacheReadTokens, cacheWriteTokens, promptTokens)` visible on window total + every rollup row + workflow levels; `ToNum` on bigint tokens; provider/model breakdown via `By Model` + workflow panel.
- [ ] No functional regression — category folders, filters/sorts/pagination/CRUD/bulk/YAML/deep-links remain identical; only split + Recurring Items type are intentional changes.

## 6. Non-Functional Review Checklist

- **Scale 10x (10k usage_records, 100 workflows):** Cost explorer filters/sorts are client-side over fetched page (100 rows) — O(n log n) trivial; backend `GetCost/GetUsage/GetWorkflowCosts` already paginated/limited (pageSize 100, workflow run limited). Telemetry trace/log panels capped at 50/100 with degraded polling. No N+1 beyond `taskIds` work-item lookups (batched via `useQueries`, stale 5m). At 10x, add server-side search param rather than widen page.
- **Are we building the right thing?** Yes — split is presentation-only; data stays unified, satisfying "just separate presentation" guardrail. Alternatives (single-file param or nested URLs) would violate distinct-URL AC or flat-canonical contract.
- **Security:** No auth surface change; both routes behind existing `AppShell` session guard; `telemetry`/`aigateway` RPCs already tenant-scoped via `app.tenant_id` RLS; no new public path.
- **Observability:** Telemetry page gains native OTEL panels (replaces iframe) — reduces Grafana as single pane; cost explorer retains Postgres-as-source-of-truth copy. Both pages keep `degraded` polling for Tempo/Loki/VM unavailability.
- **Operability:** Zero DB migration for split; pure frontend move. Rollback is file revert. CI: `tsc --noEmit` + `vitest` on `cost-explorer/utils` + `make ci`.
- **Trade-offs documented:** Option A chosen for distinct URLs + decoupling; B/C rejected for AC violations and flat-canonical breakage.
- **Consistency:** Reuses `glass-panel`/`glass-menu`/`bg-mesh` tokens, `Geist` typography, `cn()` merging; reuses existing `filter*/sort*` utils with deterministic tie-breakers and `finished` null-last semantics.

## 7. Handoff to Next Steps

- **Design Approver:** Confirm telemetry retains `OverviewPanel(dashboard telemetry widgets)` + native traces/logs/metrics (GrafanaEmbed fallback) and drops cost tab; confirm cost-explorer owns full drill-down with fresh-vs-cache split + provider/model/work-item attribution; confirm dashboard tile links.
- **Engineer:** Execute §4 file map; transplant without logic change; update placeholder copy; wire dashboard Links; run `npm run build` + `vitest frontend/src/components/cost-explorer/utils.test.ts` + manual nav audit per §5; verify no `telemetry.tsx` cost import remains via `grep -r "useGetCost\|WorkflowCostPanel" frontend/src/routes/telemetry.tsx`.
- **PR Reviewer / QA:** Verify §5 checklist, no console routing errors, `routeTree.gen.ts` untouched, Ask Orchicon panel does not appear on `/telemetry` or `/cost-explorer`, cache-vs-fresh split visible, glass styling present.

---
*Architecture notes live at `architecture-notes/4-split-cost-explorer-from-telemetry-dedicated-overview-sections.md` (this file). Downstream worker summary will reference this path.*
