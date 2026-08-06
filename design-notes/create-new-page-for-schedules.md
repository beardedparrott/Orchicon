# Design Notes — Create a New Page for Schedules

**Work item:** Create a new page for schedules
**Branch:** `feat/create-new-page-for-schedules`
**Step:** 2 — UI Design Architect
**Date:** 2026-08-06

This document is the authoritative UI/UX design spec for the Schedules page.
The implementation worker (frontend engineer) should transcribe it mechanically.
See `architecture-notes/create-new-page-for-schedules.md` for the ADR.

---

## 1. Context / problem statement

Orchicon has no single place to see *what is scheduled to run and when*.
Scheduling today is a hidden side effect of work items: a task/subtask with
`scheduled_start_at` set and status `scheduled` (10) is fired by the
`ScheduledRunReconciler` when it comes due, which starts the bound workflow run
(`internal/scheduler/scheduled_run_reconciler.go`). Users must open Work Items
and mentally filter for the purple `scheduled` pill to find what's coming up.

The ask: a **Schedules** page that shows all currently scheduled work items in
chronological order with their next runtimes, looks modern and polished while
mimicking the other Orchicon pages (standard filter bar), shows a history of
previous run items (default view = Upcoming), and has clickable links to the
work item and workflow. It must also be **structurally ready for recurring
scheduled tasks, which are not built yet** — the UI must not invent recurrence
data, but the layout/contract must not need rework when a recurrence field
lands.

## 2. Scope

- `frontend/src/routes/schedules.tsx` — new route (page shell, filter bar,
  Upcoming|History toggle, agenda timeline, history list, page-local subcomponents).
- `frontend/src/components/app-shell.tsx` — add nav entry **Schedules**.
- `frontend/src/routeTree.gen.ts` — regenerated (`npm run routes`), not hand-edited.
- Docs per AGENTS.md: `DOCUMENTATION.md` (frontend routes/section),
  `UPDATES.md` (new row), `README.md` (Last Release Changes).
- **No backend/proto/DB changes.** No new design tokens (reuse the existing
  theme system). No new npm deps.

## 3. Data model mapping (decision D2 — see ADR)

All data comes from **existing generated Connect-ES clients** (invariants #1/#2:
no business logic in the frontend, no hand-written API URLs).

| View | Definition | Source | Sort |
|---|---|---|---|
| **Upcoming** (default) | Work items with `status = WORK_ITEM_STATUS_SCHEDULED (10)`. Next runtime = `scheduled_start_at`. | `useListWorkItems("", { status: 10 })` from `@/api/workItems` | Client-side by `scheduledStartAt` **asc** (server `sort_by` only supports title/priority/created_at). |
| **History** | Work items that ran before: `scheduled_start_at` set AND status terminal (`6 succeeded | 7 failed | 8 cancelled`). "Previous run items." | `useListWorkItems("")` (all, pageSize 1000) filtered client-side to `scheduledStartAt` set + terminal status | Client-side by `scheduledStartAt` **desc** (most recent run first). |

Rationale for client-side history filter: `ListWorkItemsRequest` accepts exactly
one `status` filter, so a terminal-status union cannot be fetched in one call.
A broad fetch + client filter is honest and small at current scale. **Future
work** (note, not this PR): a backend `scheduled_only`/`status[]` filter or a
dedicated Schedules RPC would replace the client filter — the page isolates the
fetch behind the existing `useListWorkItems` hooks so this swap is localized.

### Link targets (acceptance criterion "clickable links")

| What | Where | Route |
|---|---|---|
| Work item | every card | `/work-items/$id` (`item.id`) |
| Workflow template | when `item.workflowId` set | `/workflows/$id` |
| Workflow run | History when `item.workflowRunId` set | `/workflows/$workflowId/runs/$runId` |

If neither workflow id is set (rare for scheduled items — scheduling is a
workflow-bound feature), render a muted "unbound" chip instead of a dead link.

## 4. Layout & visual design (decision D4)

Mimic the shared page anatomy (compare `work-items.tsx`, `executions.tsx`):

```
┌────────────────────────────────────────────────────────────────┐
│ Schedules                                [LiveClock 14:32:05]  │  header row
│ Upcoming runs of scheduled work items, in chronological order. │
├────────────────────────────────────────────────────────────────┤
│ [ Upcoming ] [ History ]        ← segmented toggle (default Upcoming) │
│ Search…  [Project ▾] [Kind ▾] [Sort ▾] [Asc/Desc ▾]            │  filter bar
│ ☐ N of M selected         [ Cancel N selected / Delete N ]     │  (standard pattern)
├────────────────────────────────────────────────────────────────┤
│ Stats strip (Upcoming only, optional polish):                  │
│   ● 4 upcoming   ● next 14:30 · in 12m   ● 2 due today         │
├────────────────────────────────────────────────────────────────┤
│ AGENDA TIMELINE (Upcoming):                                    │
│   Today ───────────────────────────────────────────────        │
│   │ 14:30 ● Task title ……… [Next run 14:30] [in 12m] [One-time]│
│   │ 16:00 ● Subtask title … [Next run 16:00] [in 1h 38m]       │
│   Tomorrow ───────────────────────────────────────────────     │
│   │ 09:00 ● Task title ……… [Next run tomorrow 09:00] [in 19h]  │
│ HISTORY (most recent first, reversed rail):                    │
│   │ 08:02 ● Task title ……… [Succeeded] [Ran Aug 6 08:02]       │
└────────────────────────────────────────────────────────────────┘
```

### 4.1 Header
- `<h1>` "Schedules" (`text-2xl font-semibold tracking-tight`) + muted
  description, matching every list page.
- **LiveClock** (the requested "fancy" feature): `font-mono tabular-nums`,
  local time `HH:MM:SS` + date, ticking every 1s, with a small pulse dot
  (CSS `animate-pulse`, disabled under `prefers-reduced-motion`). `role="timer"`,
  `aria-label="Current time"` — NOT a live region (never announce every second).

### 4.2 View toggle
- Segmented control styled exactly like the Tree|Board toggle in
  `work-items.tsx` (border-wrapped, `bg-accent` for active, `aria-pressed`).
- Backed by URL search param `view` (`"upcoming" | "history"`, default
  `"upcoming"`) via `validateSearch` + `Route.useSearch()` — deep-linkable,
  and satisfies "default view is on Upcoming".

### 4.3 Filter bar (AGENTS.md UI-consistency rule — replicate exactly)
Order and styles mirror `executions.tsx` (`Input`, `select` with
`h-9 rounded-md border border-input bg-transparent px-3 text-sm shadow-sm`):
1. **Search** Input — "Search title…" → passes `search` to the query (server-side).
2. **Project** select — All / projects (`useListProjects`), like work-items.
3. **Kind** select — All / **only schedulable kinds**: Task (3), Subtask (4),
   and recovery kinds (5–8) if present. Do not list Epic/Feature.
4. **Sort** select — Upcoming: `next run` (asc default | desc); History:
   `last run` (desc default | asc).
5. **Select-all checkbox** + **selection count** label ("N of M selected").
6. **Bulk action button** appears when ≥1 selected:
   - Upcoming: **Cancel N selected** → per-item `useDeleteWorkItem` (soft delete;
     the server transitions the item to `cancelled` — cancelling a schedule
     cancels the scheduled work item). `window.confirm` first (existing pattern).
   - History: **Delete N selected** → `useBatchDeleteWorkItems` (hard delete).
     `window.confirm` first.
7. Per-item checkbox on every card row; clicking the card body navigates.

### 4.4 Agenda timeline (Upcoming — the core "in order" deliverable)
- Left rail: time markers + vertical connector line (`border-l` on a rail
  column; rail hidden on the smallest screens or condensed to a leading dot).
- **Date group headers**: "Today", "Tomorrow", then weekday + date
  ("Wed, Aug 12") — chronological agenda grouping.
- Each scheduled item is a card (`Card`/`CardContent`, `hover:bg-accent` like
  executions rows):
  - **Kind dot** — small filled circle colored by work item kind, reusing the
    work-items KindBadge palette: Epic `purple`, Feature `indigo`, Task `blue`,
    Subtask `cyan`, recovery kinds `amber`/`rose`. (Do NOT invent new hues;
    consistency with Work Items wins over the canvas `--kind-*` set.)
  - **Title** — `<Link to="/work-items/$id">`, truncating, font-medium.
  - **KindBadge** (letter chip) + **StatusPill** (`scheduled`) — reuse/promote
    the helpers from `work-items.tsx` (extract to a shared location if needed,
    or duplicate page-locally; prefer minimal churn).
  - **Project name** — muted small text (from `useListProjects` id→name map).
  - **Workflow chip** — link `/workflows/$id` when `workflowId` set, small
    outline chip with the workflow id truncated.
  - **Next run** — absolute time (`14:30`) + **CountdownChip** ("in 12m",
    "in 1h 38m", "now" for items within the fire window). Driven by a page-level
    `now` state (see §6). `aria-label="Next run in 2 hours 13 minutes"`.
  - **Frequency slot** — see D5 below.

### 4.5 History (default hidden; reached via toggle)
- Reversed rail, most recent first.
- Each card: kind dot + title (→ work item), terminal **StatusPill**
  (Succeeded/Failed/Cancelled — reuse work-items colors), "Ran {date}"
  timestamp, workflow chip (→ `/workflows/$id`), and when `workflowRunId` is
  set a "Run" chip linking `/workflows/$workflowId/runs/$runId`.

### 4.6 Empty / loading / error states (mirror `executions.tsx`)
- Loading: `"Loading…"` muted text.
- Error: `text-destructive` "Failed to load schedules: {String(error)}".
- No projects: guidance `Card` ("No project selected — create a project first").
- Upcoming empty: `Card` with `SearchX` icon, "No upcoming schedules" +
  description ("Scheduled work items will appear here. Set a scheduled start
  time on a task or subtask.").
- History empty: `Card` with `SearchX` icon, "No past runs yet".

## 5. Recurring-task readiness (decision D5)

The eventual recurring scheduled task is a **backend** feature (likely a
recurrence/cron field on the work item, fired by the ScheduledRunReconciler
evolution). The UI must be ready without pretending it exists:

- Every card reserves a right-aligned **Frequency slot** (`#frequency`):
  - Today it renders a subtle muted chip **"One-time"** (derived — all
    scheduled items today are one-time).
  - When the backend adds a recurrence field, ONLY a single helper
    `recurrenceBadge(item): string | null` changes; the slot already exists,
    so the card layout does not reflow.
- **Contract note for the future field**: `scheduled_start_at` remains the
  *next* effective runtime for a recurring item (the scheduler advances it),
  so "Next run" semantics hold unchanged. Recurring items stay in the Upcoming
  view after each fire; history accumulates per occurrence. Document this in
  the eventual PR that adds recurrence.
- Do **not** render fake recurrence rows, toggles, or a "New schedule" form
  (no backend to save them to).

## 6. Live data behavior (decision D9)

- **Upcoming query**: `refetchInterval: 5_000` (matches `useListWorkItems`
  default). When an item fires, the reconciler moves it `scheduled → running`,
  the refetch drops it from Upcoming, and it appears in History once terminal —
  the "it ran" moment is self-evident without a toast.
- **History query**: `refetchInterval: 30_000` (terminal data is stable; no
  need for aggressive polling).
- **`now` ticker**: one page-level `useState(Date.now())` + `setInterval`
  (1s) drives LiveClock, CountdownChips, and the stats strip. Use
  `document.visibilitychange` to pause the interval when the tab is hidden
  (browsers throttle background timers anyway — make it explicit and cheap).
- Timestamps: `new Date(Number(ts.seconds) * 1000)` — the established pattern
  (`Timestamp` proto → ms). Render in local time.

## 7. Component architecture (decision D6)

- **Route file** `frontend/src/routes/schedules.tsx` holds the page + page-local
  subcomponents, following `work-items.tsx` (TreeView/KanbanBoard in-file) and
  `executions.tsx`:
  - `SchedulesPage` — search state, `view` from URL, `selected: Set<string>`.
  - `FilterBar` (or inline), `UpcomingAgenda`, `HistoryList`, `ScheduleCard`,
    `CountdownChip`, `KindDot`, `LiveClock`, `StatsStrip` (optional).
- Reuse existing UI kit: `Button`, `Card`, `Input` from
  `@/components/ui/*`; `cn` from `@/lib/utils`; `lucide-react` icons
  (`SearchX`, `Clock`, `CalendarClock`, `Trash2`, `Ban` for cancel).
- Extract shared helpers (`StatusPill`, `KindBadge`) from `work-items.tsx`
  only if also refactoring that file is clean — otherwise duplicate page-locally
  (they are ~30 lines). Prefer **no churn on untouched pages**.
- `routeTree.gen.ts` is generated — run `npm run routes` (`tsr generate`)
  in `frontend/` after creating the route file, BEFORE `tsc`/`make ci`.

## 8. Accessibility (WCAG 2.2 AA floor)

- Contrast: all text via existing tokens (foreground/muted-foreground/card).
  Kind dots: ≥3:1 against card background — the work-items palette passes
  (verified in the theme palette ADR); if a dot fails on a theme, fall back to
  the `--kind-*` HSL accents which are verified ≥3:1 on the lightest dark bg.
- Structure: `<h1>` per page; agenda uses `<ul>`/`<li>` (`list-none`) with
  link titles; date headers are `<h2>`/`<h3>`.
- Keyboard: everything is a link/button/select/input — no custom widgets;
  ensure `focus-visible` ring (existing `--ring` token; don't remove outlines).
- Segmented toggle: native buttons with `aria-pressed`; active view
  `bg-accent text-accent-foreground`.
- LiveClock: `role="timer"`, `aria-label="Current time"`, content NOT in an
  aria-live region (silent per-tick updates). CountdownChip: static
  `aria-label` ("Next run in 2 hours 13 minutes") recomputed on render — the
  updated label is announced only on focus/read, never spammed.
- Selection count: `aria-live="polite"` (matches existing pages' behavior).
- Motion: only `transition-colors` (global) + a subtle clock pulse gated by
  `prefers-reduced-motion` (`motion-reduce:animate-none`).

## 9. Responsive behavior (decision D8)

- **< md (mobile)**: filter bar wraps; rail hidden — cards show a leading kind
  dot; clock compact (`HH:MM`); card content stacks (title block above meta
  block); bulk action stays visible but full-width.
- **≥ md**: rail + time markers visible; card content
  `flex flex-col sm:flex-row sm:items-center sm:justify-between` (executions
  pattern); meta (next run, countdown, frequency) right-aligned.
- **≥ lg**: full-width agenda; stats strip on one line.
- No horizontal scroll; truncate titles/ids with `truncate`/`break-all font-mono`
  for ids (established pattern).

## 10. Implementation checklist (transcribe mechanically)

1. Create `frontend/src/routes/schedules.tsx` per §4–§9.
2. Add nav entry: `{ label: "Schedules", to: "/schedules" }` in
   `frontend/src/components/app-shell.tsx` after **Work Items**.
3. `cd frontend && npm run routes` to regenerate `routeTree.gen.ts`.
4. Wire URL search: `view` (`upcoming` default) + `projectId` optional —
   `validateSearch` + `Route.useSearch()` (pattern: `executions.tsx`).
5. Data: `useListWorkItems` (status 10 for Upcoming; all + client filter for
   History), `useListProjects` for the project map, `useDeleteWorkItem`/
   `useBatchDeleteWorkItems` for bulk actions.
6. Docs: DOCUMENTATION.md (frontend routes), UPDATES.md (new table row),
   README.md (Last Release Changes) — per AGENTS.md, before the final commit.
7. Verify (see §11).

## 11. Verification checklist (implementation)

1. `cd frontend && npm run routes && npm run build` — typecheck + vite build.
2. `make ci` passes (frontend lint/typecheck gate).
3. Seed a scheduled item (task/subtask with `scheduled_start_at` in the
   future + status `scheduled`, bound to a workflow) via the existing UI or
   API; verify:
   - `/schedules` renders it in Upcoming, in chronological order, with next-run
     time + countdown ticking.
   - Clicking the title → `/work-items/$id`; workflow chip → `/workflows/$id`.
   - Toggle History → seeded past run items render with terminal pills and
     links; default page load is Upcoming (no query string).
   - Filter bar: search narrows, project/kind filters work, sort asc/desc.
   - Bulk select + Cancel (Upcoming) / Delete (History) with confirm dialogs.
   - Empty states render when no scheduled/terminal items exist.
4. Browser (Playwright + **Chrome** — never Firefox, per AGENTS.md):
   - Check at 375px, 768px, 1280px widths.
   - Check a light theme (e.g. `zinc`) and a dark theme (e.g. `midnight`);
     toggle mode mid-page; verify clock, dots, pills, focus rings.
   - `getByRole` smoke: h1 "Schedules", nav link, segmented toggle,
     `role="timer"` clock.
5. Confirm `routeTree.gen.ts` diff contains only the new `/schedules` route.
6. No console errors; no new dependencies in `package.json`.

## 12. Out of scope / notes

- No backend, proto, migration, or API changes (history filtering is
  client-side by design; see §3 future work).
- No "New Schedule" creation form (recurring schedules are not built; creating
  one-time scheduled work items already exists on the Work Items flow).
- No execution-duration/cost columns in History v1 (work item detail page
  already surfaces executions; adding per-run stats would require N+1
  execution fetches — deliberate deferral).
- Nav placement is a judgment call: **Schedules after Work Items** because a
  schedule IS a work item in time. Move to after Workflows if product prefers.
