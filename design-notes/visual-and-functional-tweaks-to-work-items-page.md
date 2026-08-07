# Visual and functional tweaks to Work Items Page — Design

UI Design Architect step output. Read this before implementing; it is the
source of truth for the design decisions, interaction patterns, state
schema, accessibility floor, and acceptance criteria for this work item.

Related prior design: `design-notes/complete-ui-and-functionality-overhaul-of-work-item-page.md`
(cleaned per UPDATES #86 — the ADR numbers referenced in code comments
(ADR-1…ADR-9) came from it; this doc adds ADR-WI-1…ADR-WI-6).

---

## 1. Context

The Work Items page (`frontend/src/routes/work-items.tsx`) has two views:

- **Tree view** (`work-items-tree.tsx`): Epic → Feature → Task → Subtask
  hierarchy. Expand/collapse is per-node **local `useState`** — state is
  lost on navigation/remount.
- **Board view** (`work-items-board.tsx`): Kanban, one column per status,
  dnd-kit drag & drop. Parents render children with expand/collapse
  chevrons, **default expanded** (`useState(true)`). Drag moves exactly one
  card; there is no multi-drag.
- **Filters** (`work-items-filter-bar.tsx`): Status and Type are
  **single-select `<select>`s**; sort is two selects. No persistence —
  everything resets on reload. View toggle (Tree|Board) is also local.

Work item requirements:

1. List view: expand/collapse of parents persists across visits.
2. Board view: parents collapsible, **default collapsed**; drag multiple
   selected items to another column at once.
3. Both views: multi-select status + type filters (checkboxes in the
   dropdown); filters saved immediately and restored on return; last
   clicked view (tree or board) saved as default.

Non-negotiables from AGENTS.md: no business logic in the frontend (view
preferences are presentation state, not business logic); list pages must
follow the shared pattern (search + filter dropdowns + select-all +
count + bulk action); WCAG 2.2 AA; tokens over magic values.

---

## 2. Decisions (ADRs)

### ADR-WI-1: Persist view state client-side in localStorage (versioned), not server-side

**Context.** We need expand/collapse, filter selections, and the default
view to survive navigation and reload. Three storage options were
considered: (a) server-side per-user preferences (new table + migration +
RPC + auth plumbing), (b) URL search params, (c) localStorage.

**Decision.** localStorage with **versioned JSON envelopes** per key
(`{ v: 1, ... }`), written through on every change ("saved immediately").
This matches the app's existing pattern (`theme-store.ts` persists
`orchicon_theme`/`orchicon_mode` in localStorage; `settings.proto` §12
documents appearance as "client-side, persisted in localStorage"). No
server change. Read once at route mount via lazy `useState` initializers.

**Consequences.** (+) zero backend scope, works offline, trivial to
version/migrate. (−) not synced across devices; per-browser only (fine —
"when you come back to the view" implies same browser). URL params were
rejected because expand sets don't belong in URLs and the "last view is
the default" semantics don't fit; noted as a future enhancement for
shareable filter URLs.

### ADR-WI-2: Multi-select filters = generic `MultiSelect` checkbox dropdown in `components/ui`

**Context.** Status/Type filters must become checkbox lists in a dropdown.
Options: (a) custom-built disclosure + listbox, (b) Radix
`DropdownMenu.CheckboxItem`, (c) native `<select multiple>`.

**Decision.** A reusable `MultiSelect` primitive (`components/ui/multi-select.tsx`)
built on `@radix-ui/react-dropdown-menu` (CheckboxItem), styled with
existing tokens (`border-input`, `bg-popover`, `focus-visible:ring-2
ring-ring`). Radix is already an accepted dependency family
(`react-tooltip`, `react-slot`); the "lean primitives" comment in
`toast.tsx` is about not baking business logic into UI components, not
about avoiding battle-tested a11y libs. Put it in `components/ui/` so the
other list pages (Approvals, Executions, Workers, Policies — AGENTS.md
UI-consistency mandate) can adopt it later. Native `<select multiple>` is
rejected: no visible checkboxes, Ctrl+click semantics, poor touch UX.

**Consequences.** (+) correct keyboard/focus management out of the box
(arrows, Escape, typeahead, click-outside), testable, reusable. (−) one
new dependency (small; same family as existing). If the team prefers zero
new deps, a hand-rolled menu is acceptable **provided** the WAI-ARIA
listbox pattern + focus return are implemented exactly as specified in §5.

### ADR-WI-3: Expand/collapse persisted per view; board defaults collapsed; tree keeps filter-auto-expand

**Context.** Tree expand state is local today; board defaults expanded.
The requirement flips the board default to collapsed and persists both.

**Decision.** Store **expanded id sets** per view, per project
(`treeExpanded.<projectId>`, `boardExpanded.<projectId>`), default empty
(= all collapsed). A node is expanded iff `expandedIds.has(id)`.
- Board: replace `useState(true)` with the persisted set → default
  collapsed. Existing chevron + `aria-expanded` behavior unchanged.
- Tree: `expanded = filterActive ? ancestorIds.has(id) : expandedIds.has(id)`
  — preserves today's file-explorer auto-expand of ancestors while a
  filter is active; manual persisted state applies once the filter
  clears. The chevron click still writes the set (visual is overridden
  while a filter is active — same as today's behavior).
- Persisted expand sets are **not** cleared when filters/search/sort
  change (unlike selection, which clears via `resetKey`) — they are
  per-project browsing state.

**Consequences.** (+) predictable, minimal diff, satisfies "keep their
expanded or collapsed views". (−) per-view sets mean expanding in tree
doesn't expand the same parent in board — intentional (different
browsing contexts), documented. Stale ids (deleted items) are harmless
no-ops; optional read-time pruning noted in §7.

### ADR-WI-4: Multi-drag moves the selected set; partial-success semantics; batch mutation behind one function

**Context.** Board drag currently moves one card. Requirement: drag
multiple selected items at once. No `BatchUpdateWorkItemStatus` RPC
exists; server accepts any validated status (gates are frontend-advisory —
ADR-3).

**Decision.** Interaction: when the dragged card **is** in `selected` and
`selected.size > 1`, the drop applies to the whole selection; otherwise
exactly the dragged card. Descendants of a selected parent are **not**
auto-included (status change is per-item, matching today). The DragOverlay
shows the active card plus a "+N" count badge during multi-drag.
Validation: per-item advisory gates (target not in
`MANUALLY_UNMOVABLE_STATUSES`; blocked item → Ready; kind restrictions)
are pre-checked for the whole set; **valid items move, invalid items are
skipped** with an explicit toast summary ("Moved 3 to Ready · Skipped 2:
blocked by X, system-managed"). Abort-all was considered and rejected: one
blocked item should not dead-end a 10-item drag.

Mutation: encapsulate in one `moveItems(ids, targetStatus)` function so
the transport is swappable. **Preferred: a batch RPC** (proto below) —
atomic, one outbox write, one event, one cache invalidation, consistent
with the transactional-outbox invariant. **Acceptable fallback (frontend-
only): `Promise.all` of the existing `updateWorkItem` mutation** — each
write still goes through the outbox, partial failure is already tolerated
by the partial-success UX. Choose based on whether a backend step remains
in the workflow; the UI contract is identical.

**Consequences.** (+) powerful bulk workflow; keyboard parity (see
ADR-WI-5); partial-success avoids dead-ends. (−) non-atomic fallback emits
N events; batch RPC requires proto + codegen + handler if chosen.

### ADR-WI-5: Bulk "Move to…" action in the selection toolbar (keyboard parity)

**Context.** Drag is a pointer enhancement. Keyboard/SR users can already
move one card via the per-card "Move to…" select, but multi-move had no
non-drag path.

**Decision.** When `selectedCount ≥ 1`, the filter bar's selection area
shows a **"Move to…"** select (same styling as the existing per-card
`MoveToMenu`) next to the Delete button, operating on the selected set via
the same `moveItems` + validation path. This mirrors the AGENTS.md list-
page pattern (selection count + bulk action appears when ≥1 selected).

**Consequences.** (+) multi-move is fully accessible without a mouse;
reuses the exact per-item gates; one code path for drag and menu moves.
(−) one more control in the selection area (kept compact, text-[11px]
select like the card's).

### ADR-WI-6: Filter semantics — OR within a group, AND across groups, empty = all; search/sort included in the envelope

**Context.** With multi-select filters we must define composition and what
an empty selection means.

**Decision.** Within Status (and within Type) the selected values compose
with **OR** (item matches if its status ∈ selected). Across Status/Type/
search they compose with **AND** (current behavior). Empty set = no
filter for that group ("All statuses"/"All types"). The persisted filter
envelope also carries `search`, `sortBy`, `sortOrder` so "last filters"
means the whole visible state (the requirement's wording). Flagged in §7:
if the product wants narrower scope, persist only statuses+kinds and reset
search/sort on load — the schema makes this a one-line change.

**Consequences.** (+) predictable, matches existing single-select semantics
generalized; one write path. (−) persisting an old search string can
surprise on return ("why is the list filtered?") — mitigated by visible
active-filter affordances in the filter bar and the empty-state message.

---

## 3. State schema (storage contract)

Keys (prefix `orchicon.workItems.`), all values versioned JSON:

| Key | Shape | Scope | Default |
|---|---|---|---|
| `orchicon.workItems.view` | `{v:1, view:"tree"\|"board"}` | global | `"board"` (current default) |
| `orchicon.workItems.filters.<projectId>` | `{v:1, statuses:number[], kinds:number[], search:string, sortBy:string, sortOrder:string}` | per project | all empty; sortBy `"created_at"`, sortOrder `"desc"` |
| `orchicon.workItems.treeExpanded.<projectId>` | `{v:1, ids:string[]}` | per project | `[]` (collapsed) |
| `orchicon.workItems.boardExpanded.<projectId>` | `{v:1, ids:string[]}` | per project | `[]` (collapsed) |

Write-through on every change (toggles, checkbox changes, view switch).
Read once at mount; re-read when `projectId` changes (per-project keys).
Wrap all `localStorage` access in try/catch (storage can throw in
private/blocked modes — the `theme-store.ts` pattern). Unknown/malformed
JSON → default (forward-compatible; the `v` field enables migrations).

New pure module `components/work-items/work-items-preferences.ts`
(parse/serialize/merge/keys) so serialization is unit-testable like
`dependency-utils.test.ts`. A `useWorkItemsPreferences(projectId)` hook
(or plain `useState` initializers in the route) exposes the four slices.

## 4. Component architecture

**Changed**
- `routes/work-items.tsx` — owns persisted view/filters/expand state;
  `statusFilter: string` → `statuses: number[]`, `kindFilter` →
  `kinds: number[]`; computes `filterActive = statuses.length>0 ||
  kinds.length>0 || search!==""`; passes expand sets + toggles to
  Tree/Board; adds bulk Move-to + `moveItems`.
- `components/work-items/work-items-filter-bar.tsx` — replace the two
  `<select>`s with `<MultiSelect>` (label summaries: "All statuses" /
  "Status: Ready, Failed (2)"); prop types become arrays; add bulk
  Move-to select when `selectedCount ≥ 1`.
- `components/work-items/work-items-tree.tsx` — `TreeNode` takes
  `expandedIds` + `onToggleExpand`; drop local `userExpanded`;
  `expanded = filterActive ? ancestorIds.has(id) : expandedIds.has(id)`.
- `components/work-items/work-items-board.tsx` — `HierarchyNodeComponent`
  takes `expandedIds` + `onToggleExpand`, default collapsed; multi-drag in
  `handleDragStart`/`handleDragEnd` (compute move set; overlay count
  badge); batch move via `moveItems`.
- `components/work-items/dependency-utils.ts` — `filterItemsByKindStatus`
  and `buildTreeData` accept `number[]` (or `Set<number>`) for kind/status;
  update tests.

**New**
- `components/ui/multi-select.tsx` — generic checkbox dropdown
  (`label`, `options: {value,label}[]`, `selected: Set<number>|string[]`,
  `onChange`, optional "Select all/Clear" footer). Reusable across list
  pages.
- `components/work-items/work-items-preferences.ts` — storage keys +
  parse/serialize/merge + `useWorkItemsPreferences` hook.
- `components/work-items/batch-move.ts` (or hook) — `moveItems(ids,
  targetStatus)` encapsulating the batch RPC or Promise.all fallback,
  per-item advisory validation, partial-success result
  `{moved:number, skipped:{title,reason}[]}` for toasts.

**Unchanged**: `use-work-item-selection.ts` (selection already shared and
cleared on `resetKey`), `work-item-meta.ts` (option arrays reused),
`work-item-card.tsx`, `work-item-badges.tsx`.

## 5. Accessibility floor (WCAG 2.2 AA)

- **MultiSelect dropdown**: trigger `aria-haspopup="listbox"` +
  `aria-expanded`; panel `role="listbox"` `aria-multiselectable="true"`,
  items `role="option"` with `aria-selected` (Radix CheckboxItem provides
  this); keyboard: Enter/Space toggles, ArrowUp/Down moves, Escape closes
  **with focus returned to the trigger**; visible focus via the app's
  `focus-visible:ring-2 ring-ring`; click-outside closes. If hand-rolled,
  implement exactly this contract.
- **Filter results announced**: the existing `aria-live="polite"`
  selection/count label stays; ensure it re-announces when the filtered
  count changes.
- **Multi-move without a mouse**: bulk "Move to…" select (ADR-WI-5) is the
  canonical keyboard path; drag remains an enhancement. Per-card Move-to
  stays.
- **Expand/collapse**: existing buttons keep `aria-expanded` +
  `aria-label` ("Expand {title}"/"Collapse {title}"); board default
  collapsed is reflected in `aria-expanded=false` on first render.
- **View toggle**: `aria-pressed` already present; keep focus on the
  toggle across view switches (do not move focus into the list).
- **Multi-drag semantics**: the sortable wrapper's `aria-roledescription`
  stays "draggable"; when multi-dragging set the aria-label to
  "Dragging N items" via dnd-kit attributes.
- **Contrast/motion**: reuse existing tokens only (badge/pill classes from
  `work-item-meta.ts` are AA-verified); chevron rotation already uses
  `transition-transform` — add `motion-reduce:transition-none` where new
  transitions are introduced.

## 6. Responsive behavior

- Filter bar already wraps (`flex-wrap`); the two `MultiSelect`s join the
  wrap flow and must keep `min-w`/height consistent with the existing
  `h-9` controls; panels anchor `bottom-start` with `max-h-[320px]`
  `overflow-y-auto` (9 status options) and `max-w` limited so they never
  overflow the viewport on 375px screens.
- Board multi-drag works on touch (PointerSensor with existing distance
  3 activation constraint); unchanged at all breakpoints. Columns already
  share width via `flex-1`; tree keeps `min-w-[640px]` + `overflow-x-auto`.

## 7. Open questions / flags for downstream workers

1. **Batch RPC vs client loop (ADR-WI-4)**: the preferred
   `BatchUpdateWorkItemStatus` requires proto + codegen (Go+TS) + handler
   (validate per id, per-item `updated`/`skipped` responses, one outbox
   write). If no backend step remains, ship the Promise.all fallback —
   identical UI. Spec for the RPC:
   `rpc BatchUpdateWorkItemStatus(BatchUpdateWorkItemStatusRequest) → BatchUpdateWorkItemStatusResponse`;
   request `{tenant_id, ids, status, request_id}`; response
   `{repeated WorkItem updated, repeated {id,title,reason} skipped}`.
2. **Persist search/sort too?** ADR-WI-6 includes them; narrowing to
   statuses+kinds is a one-line change if product disagrees.
3. **New dependency**: Radix `@radix-ui/react-dropdown-menu` (ADR-WI-2).
   Confirm the team accepts it, else hand-roll per §5.
4. **Expand-set pruning**: stale ids after item deletion are no-ops;
   optional read-time intersect with loaded ids (cheap, do it if trivial).
5. **Default view for brand-new users**: stays `"board"` (matches today).

## 8. Acceptance criteria (QA checklist)

1. Tree: expand/collapse a parent → navigate away and back → state kept.
2. Board: parents render **collapsed** by default; expand one → navigate
   away and back → still expanded.
3. Board: select 2+ cards → drag one → **all** move to the target column,
   server-confirmed, overlay shows a count badge.
4. Board: drag a non-selected card → only that card moves; selection
   unchanged.
5. Board: multi-drag containing a blocked/system-managed/kind-restricted
   item → valid items move, invalid skipped, toast lists skipped titles
   and reasons.
6. Filters: check multiple statuses → OR semantics in both views
   immediately; reload → filters retained. Same for types.
7. Clear filter affordance → empty selection = all items.
8. View toggle: click Tree → reload → Tree default; click Board → reload →
   Board default.
9. Keyboard: Tab to Status trigger, Enter opens, Arrow+Space toggles
   checkboxes, Escape closes, focus returns to trigger.
10. Switching projects restores each project's own filters/expand state.
11. `npm run build` clean; unit tests updated (dependency-utils array
    filters, preferences parse/merge); `make ci` green.
