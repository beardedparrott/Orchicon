# Design Notes — Complete UI and Functionality Overhaul of Work Item Page

**Work item:** Complete UI and functionality overhaul of Work Item page
**Branch:** `feat/complete-ui-and-functionality-overhaul-of-work-item-page`
**Step:** 2 — UI Design Architect
**Date:** 2026-08-06

This document is the authoritative UI/UX design spec for the Work Items
page overhaul. The implementation worker (frontend engineer) should
transcribe it mechanically. All decisions are inline (ADR-style:
Context → Decision → Consequences) — this repo has no separate
`architecture-notes/` convention yet.

---

## 1. Context / problem statement

`frontend/src/routes/work-items.tsx` (567 lines) is a bare-minimum page:

- The **tree** is a flat recursive list of `div`s with checkboxes, expand
  buttons, a `KindBadge`, a title link, and a `StatusPill`. There are no
  row states, no hover affordances beyond `hover:bg-accent/50`, no indent
  guides, no skeletons, and no way to see dependency edges.
- The **board** is a CSS grid of 7 columns (`md:grid-cols-4 lg:grid-cols-7`)
  with tiny text-only cards. No drag & drop, no dependency indicators,
  no per-column drop targets, no WIP feel.
- **Selection is broken**: the header checkbox does a flat `setSelected(new
  Set(items.map(...)))`. When "all" is checked and the user then unchecks an
  epic, every descendant stays checked. Reported directly in the task.
- **Dependencies are invisible**: the DAG (`GetDependencyGraph`) is only
  shown on a separate read-only React Flow page; the tree/board never
  surface "blocked by" state.
- **No auto-refresh UX**: `useListWorkItems` polls at 5s, but there is no
  live indicator and no refetch-on-focus behavior is surfaced.
- **Theming debt**: `KindBadge`/`StatusPill` (and their copies in
  `schedules.tsx` and `work-items_.$id.tsx`) hardcode light-theme colors
  (`bg-purple-100 text-purple-800`, `bg-green-600 text-white`). In dark
  themes these badges are unreadable — a WCAG AA violation.

The ask: a Jira/kanban-grade revamp — slick, responsive, interactive,
fast, dynamic — with **real dependencies**, **correct filtering**,
**drag and drop**, and **auto refresh**.

## 2. Scope

In scope (frontend only — no proto/DB/Go changes):

- `frontend/src/routes/work-items.tsx` — rewritten as a thin page shell.
- `frontend/src/components/work-items/` — new shared module
  (meta, badges, cards, tree, board, filter bar, selection hook,
  dependency utils).
- `frontend/package.json` — add `@dnd-kit/core`, `@dnd-kit/sortable`,
  `@dnd-kit/utilities` (decision D4.1).
- `frontend/src/routes/schedules.tsx` and `frontend/src/routes/work-items_.$id.tsx`
  — refactor to import the shared badges (fix the whole class of
  duplicated theming debt, per AGENTS.md).
- Docs per AGENTS.md: `DOCUMENTATION.md` (frontend routes/section),
  `UPDATES.md` (new row), `README.md` (Last Release Changes).

Out of scope:

- No new RPCs, no streaming work-item events (none exists; polling is the
  auto-refresh mechanism — decision D5).
- No server-side transition enforcement changes (the server is and stays
  authoritative; the UI is advisory — decision D4.3).
- No list virtualization (decision D10).
- No WIP limits or board configuration persistence.

## 3. Data model mapping (decision D1)

All data comes from **existing generated Connect-ES clients**
(invariants #1/#2: no business logic in the frontend, no hand-written
API URLs).

| Need | Source | Notes |
|---|---|---|
| Work items for tree/board | `useListWorkItems(projectId, { search, status, sortBy, sortOrder })` — `pageSize: 1000`, `refetchInterval: 5_000` (already) | `projectId === ""` = all projects. `status` filter passed server-side. |
| Dependency DAG | `useGetDependencyGraph(projectId)` | Nodes + edges. Builds the blocked/blocking index (decision D3). |
| Projects dropdown | `useListProjects()` | Existing. |
| Status change (board drag) | `useUpdateWorkItem(projectId)` → `updateWorkItem({ id, status })` | `UpdateWorkItemRequest` already carries `optional WorkItemStatus status`. **No optimistic transition** (invariant #3 — api layer comment). |
| Bulk delete | `useBatchDeleteWorkItems()` | Existing. |
| New item | `Link to="/work-items/new"` with `projectId`/`parentId` search params | Existing flow, keep. |

### Enum values (from `work_item.proto` — do not re-invent)

**Kinds:** 1 Epic · 2 Feature · 3 Task · 4 Subtask · 5–8 recovery kinds
(render with the recovery badge style; recovery items are schedulable
but rare — treat like Task for the board columns).

**Statuses:** 1 pending · 2 ready · 3 assigned · 4 running ·
5 checkpointing · 6 succeeded · 7 failed · 8 cancelled · 9 recovering ·
10 scheduled.

## 4. Layout & visual design (decision D2)

```
┌────────────────────────────────────────────────────────────────────┐
│ Work Items                                  [New Work Item]        │  header row
│ Project → Epic → Feature → Task → Subtask. Dependencies form a     │
│ DAG between items.                          [Live 14:32:05 ◍]     │
├────────────────────────────────────────────────────────────────────┤
│ [Project ▾] [Search…           ] [Status ▾] [Type ▾] [Sort ▾] [↕] │  filter bar (wraps)
│ [● Tree] [◧ Board]          [Dependency Graph]  [▣ select-all]     │
│    2 of 34 selected                 [Delete 2 selected]            │
├────────────────────────────────────────────────────────────────────┤
│ Tree or Board (below)                                               │
└────────────────────────────────────────────────────────────────────┘
```

- Header: unchanged h1 + description; keep the **New Work Item** button.
- Add a **live indicator** in the header (reuse the Schedules LiveClock
  pattern — `role="timer"`, pulsing dot, paused on `visibilitychange`).
  It shows last-refresh time and makes auto-refresh visible (AC "Auto
  Refresh").
- Filter bar: keep every existing control (project select, search input,
  status select, kind/type select, sort select, sort-order select, Tree|
  Board segmented toggle, Dependency Graph link) — do not remove the
  AGENTS.md-mandated pattern (search input, filter/sort dropdowns,
  select-all, per-item checkboxes, selection count, bulk action).
- **Selection count + bulk delete** move into the filter bar row (shared
  between tree and board so the count is not duplicated per-view).
- View toggle: keep the `aria-pressed` segmented-control pattern used by
  Schedules (see `design-notes/create-new-page-for-schedules.md`).

### Design tokens

**No new global tokens.** Reuse the existing theme system (`--background`,
`--card`, `--muted`, `--accent`, `--border`, `--primary`, `--ring`) and the
existing `--kind-*` accent variables in `index.css`. All new colors are
**component-level** classes expressed as Tailwind alpha variants of the
kind/status hue so they adapt to light/dark automatically (decision D7).

## 5. Interaction design

### 5.1 Selection semantics — fixes the reported bug (decision D3)

**The bug:** header select-all does `setSelected(all ids)`; unchecking an
epic leaves descendants checked.

**The fix — cascade selection in the tree:**

1. Checking a node selects **its entire subtree** (node + all descendants).
2. Unchecking a node deselects **its entire subtree**.
3. A parent checkbox is **tri-state**: `checked` if the whole subtree is
   selected, `indeterminate` if some (but not all) of the subtree is
   selected, `unchecked` otherwise.
4. The header select-all operates over the **currently visible filtered
   set** (matches today's `toggleSelectAll(filteredItems)`) and is also
   tri-state: `indeterminate` when the selection is a non-empty strict
   subset of the visible set, `checked` when every visible item is
   selected.
5. **Selection lifecycle:** clear the selection whenever the project,
   status filter, type filter, search term, or sort changes (any change
   that alters the visible set). The Tree and Board views share the same
   `selected` Set and the same filters, so selection is preserved across
   a Tree/Board toggle by definition (the visible set is unchanged).
6. The selection count label (`N of M selected`) and the bulk delete
   button (destructive, appears when ≥1 selected) stay per AGENTS.md.

Implementation shape — a small hook in the shared module:

```
useWorkItemSelection(items: WorkItem[], childrenOf: (id) => WorkItem[])
  → { selected: Set<string>,
      toggle: (id) => void,          // cascades subtree
      toggleAll: (visibleIds) => void,
      allChecked: boolean, allIndeterminate: boolean }
```

### 5.2 Real dependencies (decision D4)

Dependencies are **DAG edges**, distinct from parent/child links. Surface
them in both views:

1. Fetch the graph once per project: `useGetDependencyGraph(projectId)`.
2. Derive a pure presentation index (no business logic — this is derived
   server state, invariant #1):

```
interface BlockState {
  blockedBy: Map<itemId, WorkItem[]>;   // items with an edge to this item
                                          // (this depends on them) whose
                                          // status is NOT terminal
  blocks: Map<itemId, WorkItem[]>;      // items that depend on this item
                                          // and are not yet terminal
}
```

   An item is **terminal** when status ∈ {succeeded(6), failed(7),
   cancelled(8)}. Any dependency edge (`BLOCKS` or `DEPENDS_ON`) to a
   non-terminal item makes the dependent "blocked". `RELATES_TO` never
   blocks. Put this in `dependency-utils.ts` as a pure function
   `computeBlockState(nodes, edges)` with a unit test.

3. **Tree row:** when blocked, render a chain icon (`Link2` from
   lucide-react) + count with a `Tooltip` listing the blocking item
   titles (e.g. `"Blocked by: API design, Schema migration"`). Muted
   gray for not-blocked.
4. **Board card:** same blocked chip, top-right, with tooltip.
5. **Drag & drop validation** uses this state (decision D4.3).

### 5.3 Kanban board & drag & drop (decision D5)

**D5.1 — Library.** Add `@dnd-kit/core@^6.3.1`,
`@dnd-kit/sortable@^10.0.0`, `@dnd-kit/utilities@^3.2.2`. Rationale:
the de-facto standard for kanban boards (Trello/Linear-style), first-class
keyboard sensor (accessibility), touch support, smooth animations, and no
HTML5 DnD quirks. React Flow (already a dep) is for the workflow editor
and the dependency graph — not for a kanban board.

**D5.2 — Columns.** One column per **server status**, exactly as today:
`Pending(1) · Ready(2) · Assigned(3) · Running(4) · Succeeded(6) ·
Failed(7) · Cancelled(8)`. Do **not** merge statuses into invented lanes —
the drop target must map 1:1 to a `WorkItemStatus` value. `checkpointing`
(5), `recovering` (9), `scheduled` (10) have no dedicated column; their
items render in the column of their closest active status:
`checkpointing → Running`, `recovering → Running`, `scheduled → Pending`.
This mapping lives in the shared `STATUS_META` (decision D7) with a
comment.

Layout: **horizontal scroll region** (not a squeezed grid). Columns are
`w-[280px] shrink-0` in a flex row inside `overflow-x-auto` with
`snap-x`; column headers are sticky (`sticky top-0` within the scroll
area) and show the label + a count badge. Drop highlight: the whole
column gets an inset ring + tinted background while a card is dragged
over it (`isOver` from dnd-kit).

**D5.3 — Card design (Jira-like).**

```
┌───────────────────────────────┐
│ ▣ [T] ⛓2            [running]│   checkbox · kind badge · blocked chip · status pill
│ Design the migration runner   │   title, line-clamp-2, link to /work-items/$id
│ · P3 · 2d ago                 │   meta row: priority · age
└───────────────────────────────┘
```

- Card = shared `WorkItemCard` (also used by the tree row body).
- Left edge: 3px kind-colored accent bar (`border-l-4` with the kind hue)
  for at-a-glance scanning.
- Checkbox for bulk selection (AGENTS.md pattern).
- `KindBadge` + title link + `StatusPill` + priority + relative age
  (`createdAt`).
- Blocked chip when `blockedBy` non-empty (section 5.2).
- Hover: lift (`hover:shadow-md hover:-translate-y-0.5`), cursor grab.
- Selected card: `bg-accent/60` + ring.

**D5.4 — Drop behavior (server-confirmed, invariant #3).**

- On drag end into a different column: call
  `useUpdateWorkItem(projectId).mutate({ id, status: targetStatus })`.
- **No optimistic transition** (api layer comment: "no optimistic status
  transitions — invariant #3"). Show a transient "moving…" state on the
  dragged card (e.g. reduced opacity + spinner) while the mutation is
  pending; the card returns to its origin column until the refetch lands.
- On success: toast ("Moved 'title' to Ready") + queries already
  invalidate → refetch (5s poll + invalidation → fast settle).
- On error: toast with the server message; card stays in origin column.
- **Dependency gate (advisory):** if the target column is `Ready` and the
  card is blocked (section 5.2), **block the drop** — show a toast
  "Cannot move to Ready: blocked by <titles>" and revert. The server
  remains authoritative for all other transitions; this gate prevents an
  obviously-wrong move and surfaces the DAG where the TaskReconciler
  enforces it for real.
- **Kind gate (advisory):** Epics/Features accept only the statuses
  `pending | succeeded | cancelled` (they are not schedulable); dragging
  them anywhere else shows the toast and reverts. Tasks/Subtasks accept
  any column. Define this matrix once in `dependency-utils.ts` /
  `work-item-meta.ts` (`ALLOWED_STATUSES_PER_KIND`).

**D5.5 — Keyboard/assistive alternative.** dnd-kit keyboard sensor lets
arrow-key users move cards, but provide a more discoverable fallback:
a per-card **"Move to…" menu** (small `select` or popover listing the
allowed target statuses) that performs the identical server-confirmed
mutation. This doubles as the screen-reader path.

### 5.4 Filtering (decision D6)

- Compose server-side: `status` filter goes to the API; `search` goes to
  the API; `sortBy/sortOrder` go to the API. `kind` filter stays
  client-side (API accepts one status filter only; kind filtering today
  is a client filter — keep it, it's cheap at pageSize 1000).
- The tree groups by `parentId` after filtering; when a kind/status filter
  is active the tree should **auto-expand** branches containing matches
  (like a file explorer) and highlight matches — otherwise filtered
  results can be hidden under collapsed epics.
- Select-all always operates on the **visible filtered set** (fixed in
  section 5.1).
- Search input: debounce 300ms before it reaches the query key.

### 5.5 Auto refresh (decision D7)

- Keep `refetchInterval: 5_000` in `useListWorkItems` (default already).
  Add the same to the graph query (`useGetDependencyGraph` needs an
  `opts` param with `refetchInterval` — extend it).
- Pause polling when the tab is hidden (`visibilitychange`), resume on
  visible — same as Schedules page.
- `refetchOnWindowFocus` (TanStack default) stays on.
- Live indicator (section 4) makes the refresh visible.
- No new streaming RPC: there is no work-item event stream (project
  events stream covers projects only), and building one is out of scope
  for a UI overhaul.

## 6. Component architecture (decision D8)

Route file becomes a thin shell. New shared module:

```
frontend/src/components/work-items/
  work-item-meta.ts          # KIND_META / STATUS_META (labels, hues, column
                             #   mapping, ALLOWED_STATUSES_PER_KIND),
                             #   kindDotColor(), status helpers
  work-item-badges.tsx       # KindBadge, StatusPill (theme-safe)
  work-item-card.tsx         # shared card (tree row body + board card)
  dependency-utils.ts        # computeBlockState(), isTerminal(), pure fns
  use-work-item-selection.ts # cascade selection hook (section 5.1)
  work-items-tree.tsx        # TreeView
  work-items-board.tsx       # KanbanBoard (dnd-kit)
  work-items-filter-bar.tsx  # search/filter/sort + selection count + bulk
```

- **Extract the duplicated badges.** `KindBadge`/`StatusPill` are
  copy-pasted in `work-items.tsx`, `schedules.tsx`,
  `work-items_.$id.tsx` (and `kindDotColor` is duplicated in schedules).
  After building the shared module, update Schedules + the detail page to
  import from it (AGENTS.md: fix the whole class, not one instance).
- **Theme-safe colors (a11y fix):** replace hardcoded
  `bg-purple-100 text-purple-800`-style classes with token-derived
  variants that hold up in both modes, e.g.
  `bg-purple-500/15 text-purple-800 dark:text-purple-300` (badge text
  ≥4.5:1, kind dots ≥3:1). Verify in gruvbox (light) and midnight (dark)
  at minimum.
- Pages stay routable at the same paths; `routeTree.gen.ts` regenerated
  only if routes change (they don't — no `npm run routes` needed unless
  the router complains).

## 7. Accessibility floor (WCAG 2.2 AA)

- **Contrast:** all badge text ≥4.5:1; kind accent bars/dots ≥3:1
  (non-text). Token-based colors only (section 6).
- **Tree:** rows expose `aria-expanded` on expanders, `aria-level` from
  depth, native checkboxes with `aria-label={title}`; the row is a link
  to the detail page (native focusability). Keyboard: Tab to checkbox /
  expander / link; Space toggles checkbox; Enter expands.
- **Board:** cards are links (focusable). dnd-kit Pointer + Keyboard
  sensors; the "Move to…" menu (section 5.5) is the primary assistive
  path. Drop targets announce via `aria-live`.
- **Live regions:** one `aria-live="polite"` region for the selection
  count (`"2 of 34 selected"`) and one for toasts (existing `toaster`
  component).
- **Reduced motion:** respect `prefers-reduced-motion` for card lift /
  drop animations (Tailwind `motion-reduce:` variants or a media guard).
- **Focus:** visible focus ring on all interactive elements (the
  `--ring` token already provides this — ensure new cards/menus apply
  `focus-visible:ring`).

## 8. Responsive behavior (decision D9)

- **Tree:** `overflow-x-auto` with `min-w-[640px]` inner; indent guides
  scale with depth; at 375px the row shows checkbox + expander + title
  (badges hide behind the title on very narrow widths or wrap).
- **Board:** horizontal scroll with `snap-x`; columns `w-[280px]
  shrink-0`; the toolbar wraps (already `flex-wrap`).
- **Filter bar:** wraps to 2 rows on mobile; search input grows
  (`flex-1 min-w-[160px]`).
- Verify at 375 / 768 / 1280 / 1536 like the Schedules page verification.

## 9. Performance (decision D10)

- `useMemo` the tree build and per-column grouping (already partially
  done in graph page; apply to tree/board).
- Debounce search (300ms).
- No virtualization yet: at `pageSize: 1000` with kind/status client
  filtering the render cost is acceptable; if >500 visible items becomes
  a real bottleneck, add `react-window` in a follow-up (note in docs).
- dnd-kit `measuring`/`animation` defaults are fine; avoid re-rendering
  the whole board on drag (use `useSensors` + `DndContext` per board,
  memoized cards).

## 10. Implementation checklist (maps to acceptance criteria)

| AC | Implementation |
|---|---|
| Complete revamp | Rewrite route shell + shared module (section 6); Jira-grade tree rows + cards |
| Real dependencies | `computeBlockState` from `GetDependencyGraph`; blocked chips + tooltips in tree and board; Ready-drop gate (5.2/5.4) |
| Correct filtering | Server-side status/search/sort + client kind; auto-expand on filter; tri-state select-all over visible set; cascade selection (5.1) |
| Jira/kanban board | Horizontal-scroll columns, dnd-kit drag & drop, per-column drop targets, card design (5.3) |
| Slick/responsive/interactive/fast/dynamic | Skeletons, hover lifts, tooltips, live indicator, debounced search, memoization, responsive breakpoints (4/8/9) |
| Drag and Drop | dnd-kit + "Move to…" fallback; server-confirmed mutations with toasts (5.4/5.5) |
| Auto Refresh | 5s polling (list + graph), visibility pause, focus refetch, live indicator (5.5) |

## 11. Verification (for the implementer)

1. `npm install` new deps → `npm run build` clean, no new lint errors,
   `make ci` green (frontend-only; pre-existing Go env failures excluded).
2. Seed epics/features/tasks/subtasks across projects with dependency
   edges (`AddDependency`).
3. Browser (Chrome — never Firefox) at 375/768/1280/1536, light
   (gruvbox) + dark (midnight):
   - Cascade selection: check epic → descendants checked; uncheck epic →
     all descendants unchecked; header select-all tri-state.
   - Filtering: kind + status + search compose; select-all respects the
     filtered set; selection clears on filter change.
   - Board: drag card Pending→Running; blocked card cannot move to Ready
     (toast + revert); epic cannot move to Running (toast + revert);
     "Move to…" menu works for keyboard users.
   - Auto refresh: change a status in another tab/API call → board/tree
     update within ~5s; live indicator ticks and pauses when tab hidden.
   - No console errors; toasts appear on success/failure.
4. Dark theme: verify every badge/card is readable (the old hardcoded
   colors are gone).
5. Update `DOCUMENTATION.md`, `UPDATES.md`, `README.md` per AGENTS.md.

## 12. Future work (note, not this PR)

- Server-side multi-status filter or dedicated board RPC (removes the
  client kind filter).
- Board column configuration / WIP limits (needs data).
- List virtualization at scale.
- A dedicated work-item event stream for true push updates.

---

## Appendix A — ADR-style decision records

Each significant design decision, in the required
Context → Decision → Consequences form.

### ADR-1: Rewrite as a thin route + shared `components/work-items/` module
**Context:** the 567-line route file duplicates `KindBadge`/`StatusPill`
across three files with hardcoded light-only colors; a revamp of this
size must not live in one file.
**Decision:** split into a shared module (meta, badges, card, tree,
board, filter bar, selection hook, dependency utils); route stays a
shell; Schedules + detail pages import the shared badges.
**Consequences:** single source of truth for work-item presentation;
theming debt is fixed repo-wide; small refactor churn in two other
routes (mechanical).

### ADR-2: Cascade (subtree) selection with tri-state checkboxes
**Context:** the reported bug — select-all then unchecking an epic
leaves descendants checked; flat selection is sloppy.
**Decision:** checking/unchecking a node applies to its entire subtree;
parent checkboxes and the header select-all are tri-state over the
visible filtered set; selection clears when the visible set changes.
**Consequences:** predictable, Jira-grade selection; slightly more logic
in one hook (`useWorkItemSelection`); behavior fully unit-testable.

### ADR-3: Surface the DAG — derived blocked state, no client business logic
**Context:** "real dependencies" is an acceptance criterion, but
dependencies are DAG edges invisible in both views today.
**Decision:** fetch `GetDependencyGraph` and derive a pure
`computeBlockState` index (blockedBy/blocks) for presentation; render
blocked chips + tooltips in tree and board; the board gates Ready drops
on blocked state. All rules remain advisory — the server stays
authoritative (invariant #1).
**Consequences:** dependencies are visible where users plan work; the
gate prevents obviously-wrong moves; the server's actual enforcement
(TaskReconciler) is unchanged.

### ADR-4: dnd-kit for board drag & drop
**Context:** no DnD library exists in the repo; HTML5 DnD is
touch-unfriendly and inaccessible.
**Decision:** add `@dnd-kit/core@^6.3.1`, `@dnd-kit/sortable@^10.0.0`,
`@dnd-kit/utilities@^3.2.2`; Pointer + Keyboard sensors; a per-card
"Move to…" menu as the assistive alternative.
**Consequences:** three new (small, standard) dependencies; keyboard +
touch support; drop animation for free.

### ADR-5: Server-confirmed board moves — no optimistic transitions
**Context:** the api layer documents "no optimistic status transitions —
invariant #3"; a slick board tempts optimistic DnD.
**Decision:** dragging calls `updateWorkItem({ id, status })`; the card
shows a transient moving state and stays/returns until the server
confirms via invalidation + refetch; errors surface as toasts.
**Consequences:** UI can never diverge from server state; drag settle
latency is one refetch (~instant locally); consistent with the rest of
the app.

### ADR-6: Polling-based auto refresh (5s + visibility pause + focus refetch)
**Context:** AC demands auto refresh; no work-item event stream exists
and building one is out of scope.
**Decision:** keep the existing 5s `refetchInterval` on the list query,
add it to the graph query, pause on tab-hidden, refetch on focus, and
make it visible with a live indicator.
**Consequences:** zero backend change; up to 5s staleness (acceptable —
matches Schedules precedent); a future push stream can slot in without
UI rework.

### ADR-7: Token-derived theme-safe colors; no new global tokens
**Context:** hardcoded Tailwind color classes break in dark themes
(WCAG AA violation) and the kind/status palettes are duplicated.
**Decision:** component-level alpha variants of existing hues
(`bg-purple-500/15 text-purple-800 dark:text-purple-300` style) and the
existing `--kind-*` tokens; no `index.css` token additions.
**Consequences:** all themes stay readable; no token drift; verification
must include light + dark.

### ADR-8: One column per server status in a horizontal scroll region
**Context:** 7 statuses cannot fit a squeezed grid responsively, and the
drop target must map 1:1 to a status value.
**Decision:** fixed-width columns in `overflow-x-auto` with snap; the
uncolumned statuses (checkpointing/recovering/scheduled) render in their
closest active column via `STATUS_META` mapping.
**Consequences:** honest status mapping, mobile-friendly scroll; a board
configuration feature can come later without changing the mapping.

