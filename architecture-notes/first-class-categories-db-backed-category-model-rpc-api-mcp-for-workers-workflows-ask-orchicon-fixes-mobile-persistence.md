# First-class Categories: DB-backed category model + RPC/API/MCP for workers, workflows & Ask Orchicon (fixes mobile persistence)

**Status:** Approved for implementation (Principal Software Architect, Step 1/7)
**Date:** 2026-08-28
**Work item:** First-class Categories: DB-backed category model + RPC/API/MCP for workers, workflows & Ask Orchicon
**Branch:** `first-class-categories-db-backed-category-model-rpc-api-mcp-for-workers-workflows-ask-orchicon-fixes-mobile-persistence-4yptvxhbjm7rwrnm`
**Author:** Principal Software Architect (Orchicon worker)

> Architecture summary exists at `architecture-notes/first-class-categories-db-backed-category-model-rpc-api-mcp-for-workers-workflows-ask-orchicon-fixes-mobile-persistence.md` and is the authoritative design input for Steps 2-7.

---

## 1. Context

### Bug
Mobile view loses categories for workers, workflows, and Ask Orchicon conversations on sheet close, viewport change, or reload. Three prior tasks shipped a localStorage layer (`frontend/src/lib/category-store.ts`, keys `orchicon.categories.{workers,workflows,conversations}`) that the file itself documents as "frontend-only, does NOT affect the backend data model." The fragility is architectural, not a regression.

### Why localStorage fails on mobile
- Sheet/viewport logic re-mounts route components; `useCategoryPreferences` re-reads `localStorage` per mount and its `useEffect` on `page` re-initializes state. On mobile the `Sheet` unmounts the panel, focus-trap closes, and `ensureSeeded` never re-seeds because `hasEnvelope` is already true with stale data.
- `localStorage` is per-origin per-device; not tenant-scoped, not shared across devices, not visible to workers/MCP, not audited.
- No server source of truth means reload can show empty/default categories if storage was blocked (private mode) or evicted.

### Requirements (from work item)
1. Tenant-scoped `categories` table, polymorphic `target_type` (worker|workflow|conversation), plus assignments mapping `entity_id -> category_id`. Atlas migration, idempotent.
2. **Independent sets per target_type** — no crossover. One table, rows partitioned by `target_type`, every operation scoped by it.
3. RPC: ListCategories, CreateCategory, UpdateCategory, DeleteCategory, AssignToCategory, UnassignFromCategory, ReorderCategories — same validation/tenant-isolation/audit discipline as the rest of the control plane. `target_type` required and immutable.
4. MCP/AskOrchicon tools mirroring the RPCs, same scoping.
5. Frontend: replace localStorage store with server-backed queries/mutations; collapse state may stay local; fix mobile bug.
6. Migration: best-effort seeding of existing localStorage categories+assignments into DB on first load, per target_type.
7. Tests: DB, RPC, MCP, frontend — including cross-contamination tests; `make ci` passes.

### Existing architecture to respect
- `internal/db` is the ONLY place raw SQL lives (AGENTS.md invariant #4). Every accessor takes a `pgx.Tx` + `tenant_id`; RLS (`app.tenant_id` via `BeginTenantTx` + `set_config`) is the backstop (docs/09 §8.5).
- Mutations write audit rows in the same transaction as the state change (`internal/db/audit.go` pattern: `CreateAuditEvent` inside the tenant tx).
- Proto definitions under `proto/orchicon/api/v1/` generate Connect-REST + TS clients; frontend never hand-writes URLs (`frontend/src/api/clients.ts`).
- Frontend server state lives in TanStack Query (`frontend/src/api/*.ts`); mutations invalidate query keys, no optimistic status transitions (invariant #3).
- Atlas migrations under `db/migrations/` are forward-only, `make migrate-hash` after hand-editing.

---

## 2. Decisions (ADRs)

### ADR-001: Single `categories` table partitioned by `target_type` + join table `category_assignments`

**Context:** Need tenant isolation, three disjoint sets, ordered categories, and a mapping from each entity to at most one category. Options: (A) three separate tables, (B) single table with enum discriminator, (C) JSON column on each entity.

**Decision:** One table `categories` + join table `category_assignments` (or `category_memberships`).

```sql
-- categories: tenant-scoped, partitioned logically by target_type
CREATE TABLE categories (
  id          text NOT NULL PRIMARY KEY,           -- ULID (consistent with identities, workers, workflows, conversations)
  tenant_id   text NOT NULL,
  target_type text NOT NULL CHECK (target_type IN ('worker','workflow','conversation')),
  name        text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 64),
  description text NOT NULL DEFAULT '',
  slug        text NOT NULL,                        -- derived, unique within (tenant_id, target_type)
  sort_order  integer NOT NULL DEFAULT 0,           -- explicit order for ReorderCategories (avoid "order" keyword)
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, target_type, slug),
  UNIQUE (tenant_id, target_type, name)             -- optional: enforce unique display name per set; see consequences
);
CREATE INDEX categories_tenant_target_order_idx ON categories (tenant_id, target_type, sort_order, id);
ALTER TABLE categories ENABLE ROW LEVEL SECURITY; FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON categories FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

-- assignments: at most one category per entity (enforced by UNIQUE entity side)
-- No FK to workers/workflows/conversations — target_type is polymorphic and entities may be deleted;
-- writer validates existence if desired, but deletion should not cascade categories (assignment cleanup is best-effort or FK with ON DELETE CASCADE per-type is not feasible uniformly).
CREATE TABLE category_assignments (
  tenant_id   text NOT NULL,
  target_type text NOT NULL CHECK (target_type IN ('worker','workflow','conversation')),
  entity_id   text NOT NULL,
  category_id text NOT NULL REFERENCES categories(id) ON DELETE CASCADE,
  created_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, target_type, entity_id),
  UNIQUE (tenant_id, entity_id)  -- extra guard when target_type is part of PK
);
CREATE INDEX category_assignments_category_idx ON category_assignments (tenant_id, category_id);
CREATE INDEX category_assignments_entity_idx ON category_assignments (tenant_id, entity_id);
ALTER TABLE category_assignments ENABLE ROW LEVEL SECURITY; FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON category_assignments FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));
```

**Why ULID for `id`:** Consistency with `db.NewID()` used elsewhere; sortable, no extra sequence.

**Why `sort_order` not `order`:** `order` is a reserved keyword; `sort_order` mirrors `work_items.sort_order` added in `20260809000000_work_items_sort_order.sql`.

**Why no FK from `category_assignments.entity_id`:** Workers, workflows, and conversations live in different tables with different PKs; a polymorphic FK would require three FK columns or a trigger. Assignments are organizational, not referential integrity — if an entity is deleted, its assignment row can be garbage-collected (by `ON DELETE CASCADE` from categories, plus an explicit `DELETE FROM category_assignments WHERE entity_id = $1` in the entity's `Delete*` path). This matches the current frontend behavior where deleting a category moves items to Uncategorized (deletes assignment).

**Consequences:**
- Single migration covers all three sets; adding a fourth target_type later is just a CHECK extension + RLS still applies.
- Every query MUST filter by `(tenant_id, target_type)` — the data-access helpers enforce this; the RPC layer validates `target_type` is one of the three and rejects missing/invalid values with `InvalidArgument`. Cross-contamination tests assert `ListCategories(workers)` never returns `workflow` rows.
- Ordering: `sort_order` is dense per `(tenant_id, target_type)`; `ReorderCategories` re-numbers in a single transaction (see ADR-003).
- `slug` uniqueness prevents duplicate names per set but still allows same display name across sets (e.g. "Infra" for both workers and workflows) — they are distinct rows because `target_type` differs.

**Alternatives rejected:**
- Three tables (`worker_categories`, etc.) — duplicates schema, migrations, and RPC code 3x; the requirement explicitly says "one shared table, rows partitioned by target_type."
- Embedding `category_id` on the entity row — would require altering three unrelated tables, complicates the immutable entity headers, and prevents a worker from being uncategorized cleanly.

---

### ADR-002: New `CategoryService` (proto + RPC), surface category info on list responses

**Context:** Existing services (WorkerService, WorkflowService, AskOrchiconService) each have their own list endpoints. Requirement says "Category info surfaced on the affected list responses (each response only carries its own target_type's categories)."

**Decision:** Create `proto/orchicon/api/v1/category.proto` + `category_service.proto` with a dedicated `CategoryService`. Do NOT overload the three existing services with category mutations; keep a single authority.

**Proto sketch (full field comments in the .proto):**

```proto
syntax = "proto3";
package orchicon.api.v1;

import "google/protobuf/timestamp.proto";

enum CategoryTargetType {
  CATEGORY_TARGET_TYPE_UNSPECIFIED = 0;
  CATEGORY_TARGET_TYPE_WORKER = 1;
  CATEGORY_TARGET_TYPE_WORKFLOW = 2;
  CATEGORY_TARGET_TYPE_CONVERSATION = 3;
}

message Category {
  string id = 1;
  string tenant_id = 2;
  CategoryTargetType target_type = 3;
  string name = 4;
  string description = 5;
  string slug = 6;
  int32 sort_order = 7;
  google.protobuf.Timestamp created_at = 8;
  google.protobuf.Timestamp updated_at = 9;
}

message CategoryAssignment {
  string entity_id = 1;
  string category_id = 2;
  CategoryTargetType target_type = 3;
}

service CategoryService {
  rpc ListCategories(ListCategoriesRequest) returns (ListCategoriesResponse);
  rpc CreateCategory(CreateCategoryRequest) returns (CreateCategoryResponse);
  rpc UpdateCategory(UpdateCategoryRequest) returns (UpdateCategoryResponse);
  rpc DeleteCategory(DeleteCategoryRequest) returns (DeleteCategoryResponse);
  rpc AssignToCategory(AssignToCategoryRequest) returns (AssignToCategoryResponse);
  rpc UnassignFromCategory(UnassignFromCategoryRequest) returns (UnassignFromCategoryResponse);
  rpc ReorderCategories(ReorderCategoriesRequest) returns (ReorderCategoriesResponse);
}
```

Request/response shapes:
- `ListCategories`: `target_type` required (enum, `UNSPECIFIED` rejected), optional `search`, cursor pagination (`page_token`/`page_size`), returns `repeated Category` ordered by `sort_order`.
- `CreateCategory`: `target_type` required (immutable), `name` required (1-64, trimmed), `description` optional (max 256, matches frontend limit), `sort_order` not set by caller — server appends at end (`max(sort_order)+1` within the target_type set).
- `UpdateCategory`: `id`, optional `name`, optional `description`. `target_type` is NOT updatable — if supplied and mismatches, return `InvalidArgument`.
- `DeleteCategory`: `id` — deletes category row; `ON DELETE CASCADE` removes assignments; affected entities move to Uncategorized (no assignment row). Audited.
- `AssignToCategory`: `category_id`, `entity_id`, `target_type` — validates `category.target_type == request.target_type`, validates category exists and is tenant-scoped. Upsert assignment (one category per entity — replaces any existing assignment for that entity). If `entity_id` does not exist, return `NotFound` or `InvalidArgument` depending on whether validation is strict; recommend `NotFound` with a clear message.
- `UnassignFromCategory`: `entity_id`, `target_type` — deletes assignment row (idempotent: deleting a non-existent assignment is a no-op but still audited as a no-op or returns success).
- `ReorderCategories`: `target_type`, `ordered_ids: repeated string` — must contain exactly the set of category ids for that `(tenant, target_type)` or the server re-numbers only the supplied ids and leaves others appended; choose the strict variant: the list must be a permutation of the existing ids for that target_type, otherwise `InvalidArgument`. Transactionally updates `sort_order` = index, `updated_at` = now().

**Validation / tenant isolation / audit discipline (matches `internal/db/worker.go` + `internal/db/audit.go`):**
- Every handler starts with `pool.BeginTenantTx(ctx, tenantID)` where `tenantID` comes from the auth context (never trust a client-supplied `tenant_id` except for admin paths — here the tenant is the caller's tenant). The proto's `tenant_id` field, if present, is ignored or must match the caller's tenant.
- Name: trimmed, 1-64, non-empty; description: max 256; slug: derived server-side via `slugify(name)` (lowercase, `[^a-z0-9]+` → `_`, trim `_`); on collision, append `_<shortULID>`.
- Audit: each mutation writes `audit_events` with `action = "category.created" | "category.updated" | "category.deleted" | "category.assigned" | "category.unassigned" | "category.reordered"`, `target_type = "category"`, `target_id = category.id` (or entity_id for assignments), `before`/`after` JSONB snapshots, same transaction.
- RLS is the backstop; the data-access layer is the primary isolation.

**Surfacing category info on list responses:**
Two compatible options; recommend **Option A** for minimal coupling:

- **Option A (preferred):** Enrich `ListWorkersResponse`, `ListWorkflowsResponse`, and `ListConversationsResponse` to return assignments alongside items. Minimal proto diff:
  ```proto
  // In worker_service.proto, workflow_service.proto, ask_orchicon_service.proto:
  // Add to each List*Response:
  repeated Category categories = 99;          // only the caller's target_type set
  repeated CategoryAssignment assignments = 100; // entity_id -> category_id for returned page
  ```
  The handler joins `categories` + `category_assignments` filtered by the same tenant + target_type in one extra query per list call (single round-trip via `SELECT ... WHERE tenant_id=$1 AND target_type=$2`). Frontend gets categories + assignments atomically with the list, no waterfall.

- **Option B:** Separate `ListCategories` + `ListAssignments` RPC and let the frontend join client-side. Simpler server but adds a second fetch and a race between lists.

Choose **A** — it guarantees consistency between the item list and its category overlay, avoids N+1 on the frontend, and matches the existing `WorkerListItem` enriched-row pattern (`internal/db/worker.go:ListWorkersWithActiveVersion` does a `LEFT JOIN LATERAL` to enrich in one trip).

**Consequences:**
- New Atlas migration: `db/migrations/20260829000000_categories.sql` (forward-only, idempotent `CREATE TABLE IF NOT EXISTS` + `CREATE INDEX IF NOT EXISTS` + RLS guard `DO $$ BEGIN ... EXCEPTION WHEN duplicate_object THEN NULL; END $$;` for policy).
- Regenerate proto via `buf generate` + `make proto` (if the repo has that target); commit generated Go + TS clients.
- Wire `CategoryService` into `internal/server/server.go` + Connect handler registration (same pattern as `WorkerService` / `WorkflowService`).
- Existing list handlers add a categories/assignments projection; no breaking change (new fields are additive).

---

### ADR-003: Ordering — `ReorderCategories` is transactional, `sort_order` is dense

**Context:** Frontend needs drag-reorder within a single target_type set; partial reorder (keyboard DnD) must not interleave with concurrent edits.

**Decision:** `ReorderCategories` runs in one `SERIALIZABLE` or `REPEATABLE READ` transaction:
1. `SELECT id FROM categories WHERE tenant_id=$1 AND target_type=$2 ORDER BY sort_order, id FOR UPDATE`
2. Validate `ordered_ids` is a permutation of the selected set (size matches, all ids belong to the set).
3. `UPDATE categories SET sort_order = idx, updated_at = now() WHERE id = $1` for each id in order (or a single `CASE` update).
4. Write one audit event `category.reordered` with `before: {order: [...]}, after: {order: [...]}`.

If validation fails, no rows change. Concurrent reorder attempts serialize on the `FOR UPDATE` lock — one succeeds, the other retries or surfaces `Aborted`.

**Consequences:**
- No gaps, no duplicate `sort_order` within a set.
- Frontend can optimistically show the new order but must refetch on error (no optimistic mutation beyond the drag preview).

---

### ADR-004: MCP / AskOrchicon tools mirror RPCs with identical scoping

**Context:** Requirement 4: `orchicon_*` tools `create/list/update/delete/assign` registered and tested so workers can manage categories too, same target_type scoping.

**Decision:** Extend `internal/askorchicon/tools.go` + add `internal/askorchicon/tool_categories.go`:

```go
// Tool names (snake_case, orchicon_* prefix):
// - list_categories
// - create_category
// - update_category
// - delete_category
// - assign_to_category
// - unassign_from_category   // mirrors RPC UnassignFromCategory
// - reorder_categories
```

Each tool:
- Takes `target_type` as a required string/enum (`worker|workflow|conversation`) — validated against the same enum as the RPC.
- Takes the same fields as the RPC (name/description/category_id/entity_id/ordered_ids).
- Calls the same `internal/db` helpers via `pool.BeginTenantTx(ctx, tenantID)` — no new SQL, no bypass.
- Is `Mutating: true` except `list_categories`.
- Returns JSON with `id`, `name`, `target_type`, etc., matching the proto JSON mapping.

**Registration:** Append to `allTools()` in `tools.go` (same block as `list_workers`, `create_worker`, etc.). No special casing for `AskOrchicon` conversation categories — the agent can organize its own conversations.

**Consequences:**
- The agent's tool surface never drifts from the control plane (worker rule: "If you add/change/remove a first-class entity, RPC, or user-facing capability, update the Ask Orchicon tool registry").
- Tests: `internal/askorchicon/tool_categories_test.go` asserts scoping (workers categories not visible when listing workflows), validation, and tenant isolation.

---

### ADR-005: Frontend — replace `category-store.ts` localStorage with server-backed TanStack Query

**Context:** Three pages (`workers.tsx`, `workflows.tsx`, `ask-orchicon.tsx`) currently call `useCategoryPreferences(page)` which reads/writes `localStorage` directly and holds state in `useState` seeded from `loadCategoryState`. `CategoryFolder.tsx` + `CategoryDndContext.tsx` call `onAssign(entityId, categoryId)` which mutates the local store. `CreateCategoryDialog.tsx` creates a local category.

**Decision:** Keep **collapse state local**, move **categories + assignments server-side**.

**New client (`frontend/src/api/categories.ts`):**
```ts
export const categoryKeys = {
  all: ["categories"] as const,
  list: (targetType: CategoryTargetType) => [...categoryKeys.all, "list", targetType] as const,
  assignments: (targetType: CategoryTargetType) => [...categoryKeys.all, "assignments", targetType] as const,
};

export function useListCategories(targetType: CategoryTargetType) { ... }
export function useCreateCategory()    // -> invalidates categoryKeys.list(targetType)
export function useUpdateCategory()    // rename/description
export function useDeleteCategory()
export function useAssignToCategory()  // entity_id -> category_id
export function useUnassignFromCategory()
export function useReorderCategories() // ordered_ids
```

**Generated client:** `categoryClient` from `@/api/clients.ts` (add `CategoryService` import + `createClient(CategoryService, connectTransport)` — same as `workerClient`).

**Per-page mapping:**
- `workers.tsx`: `targetType = CATEGORY_TARGET_TYPE_WORKER`
- `workflows.tsx`: `targetType = CATEGORY_TARGET_TYPE_WORKFLOW`
- `ask-orchicon.tsx`: `targetType = CATEGORY_TARGET_TYPE_CONVERSATION`

Each page:
- Calls `useListCategories(targetType)` (single query that returns categories + assignments for the current page's items — or two queries if Option B from ADR-002 is chosen).
- Derives `categorized`/`uncategorized` from the server assignment map (same shape as `getItemsForCategory` but input is server data, not `prefs.state`).
- Wires `CategoryDndContext.onAssign` to `assignToCategory.mutate({categoryId, entityId, targetType})` / `unassignFromCategory`.
- Wires `CreateCategoryDialog.onCreate` to `createCategory.mutate({name, description, targetType})`.
- Keeps `CategoryFolder`'s collapse toggle local (still `localStorage` or `sessionStorage` under `orchicon.categories.{page}.collapsed` — collapse is UX state, not domain state). The `isCollapsed` prop stays driven by the local `collapsed` Set; only `category` + `count` + `onRename`/`onDelete`/`onAssign` become server-backed.

**`frontend/src/lib/category-store.ts` becomes:**
- Retain `loadCollapsedState` / `saveCollapsedState` + `CategoryPreferences.collapsed` + `toggleCollapsed`.
- Delete or deprecate `loadCategoryState` / `saveCategoryState` / `seedAssignments` / `useCategoryPreferences`'s category/assignment mutations — replace with a thin `useCategoryCollapse(page)` hook if desired. Or keep the file as a re-export of the new query hooks for incremental migration (mark old APIs `@deprecated`).

**Mobile persistence fix — why this works:**
- Categories + assignments are now tenant-scoped server rows, fetched on every mount via TanStack Query (with `staleTime` + `refetchOnWindowFocus`). Sheet close, viewport change, and reload all re-fetch from the server — no lost state.
- TanStack Query's cache survives component remounts; the `Sheet` that unmounts the panel no longer destroys the data (it lives in the query cache keyed by `["categories", targetType]`).
- `localStorage` is no longer the source of truth — see ADR-006 for the migration that makes the old store a cache.

**Consequences:**
- No more per-page `ensureSeeded` effect — seeding is handled by ADR-006.
- `CategoryDndContext` no longer depends on a local `validTargets` derived from `categories` prop — it still does, but the categories prop is now server data, so the DnD target set is always consistent with what the server knows.
- Loading state: pages show a skeleton or the previous data (`keepPreviousData` as in `useListWorkItems`) while categories refetch.

---

### ADR-006: Best-effort one-time migration from localStorage to DB

**Context:** Existing users have categories + assignments in `localStorage` under `orchicon.categories.{workers,workflows,conversations}`. After deploy, those should appear in the DB so nothing is lost, then `localStorage` degrades to a cache.

**Decision:** On first load after deploy, each of the three pages runs a one-time seeding effect:

```ts
// Pseudocode inside workers.tsx / workflows.tsx / ask-orchicon.tsx (or in the categories hook):
const { data: serverCategories } = useListCategories(targetType);
const hasSeededKey = `orchicon.categories.${targetType}.seeded_v2`;

useEffect(() => {
  if (!serverCategories || serverCategories.length > 0) return; // server already has data — do not overwrite
  if (localStorage.getItem(hasSeededKey)) return;                // already attempted
  const local = loadCategoryState(page); // existing helper, reads orchicon.categories.{page}
  if (local.categories.length === 0 || local.categories.every(c => c.id === "cat_software_dev" && Object.keys(local.assignments).length === 0)) {
    localStorage.setItem(hasSeededKey, "1");
    return; // nothing meaningful to migrate (just the default seed)
  }
  // Fire-and-forget best-effort: create each local category in order, then assign.
  (async () => {
    for (const cat of [...local.categories].sort((a,b) => a.order - b.order)) {
      try { await createCategory({targetType, name: cat.name, description: cat.description}); } catch {}
    }
    // Re-list to get server ids, then map local ids -> server ids by name/slug
    const fresh = await refetchCategories();
    const byName = new Map(fresh.map(c => [c.name, c.id]));
    for (const [entityId, localCatId] of Object.entries(local.assignments)) {
      const localCat = local.categories.find(c => c.id === localCatId);
      if (!localCat) continue;
      const serverCatId = byName.get(localCat.name);
      if (!serverCatId) continue;
      try { await assignToCategory({targetType, entityId, categoryId: serverCatId}); } catch {}
    }
    localStorage.setItem(hasSeededKey, "1");
  })();
}, [serverCategories]);
```

**Key properties:**
- Each page seeds only its own `target_type` — workers page seeds `worker` categories, etc. No crossover.
- Seeding only runs when the server set is empty — if the tenant already has categories (e.g. created on another device), the local store is ignored (server wins).
- Failures are swallowed — this is best-effort; the user can always recreate a category manually.
- After seeding, `localStorage` is not cleared — it remains as a cache but is never read for categories/assignments again; future loads read from the server.
- The `seeded_v2` key prevents re-seeding on every reload (idempotent).

**Alternative considered:** Server-side migration that reads `localStorage` — impossible (server has no access to browser storage). Hence client-side seeding is the only viable path.

---

### ADR-007: Testing strategy — including cross-contamination tests

**Context:** Requirement 7: DB, RPC, MCP tool, and frontend component coverage — including explicit cross-contamination tests (workers categories never visible in workflows/conversations and vice versa); `make ci` passes.

**Decision:** Four test layers, each with a cross-contamination case:

1. **DB layer (`internal/db/category_test.go`):**
   - CRUD per target_type, tenant isolation (two tenants, same name, different rows), `ReorderCategories` permutations, assignment upsert/delete, cascade on category delete.
   - **Cross-contamination:** create `worker` category "Infra", create `workflow` category "Infra" (same name, different `target_type`) — assert `ListCategories(worker)` returns only the worker row; `AssignToCategory` with a workflow category id + worker target_type fails with `InvalidArgument`.

2. **RPC handler (`internal/api/category_service_test.go` or `internal/category/service_test.go`):**
   - Validation: missing `target_type`, immutable `target_type` on update, name length, description length, slug collision, `Reorder` with missing/extra ids.
   - Tenant isolation: caller tenant A cannot list/delete tenant B's categories (assert empty list / NotFound).
   - Audit: after each mutation, assert an `audit_events` row exists with correct `action`/`target_type`/`target_id`.

3. **MCP tools (`internal/askorchicon/tool_categories_test.go`):**
   - Exercise the `orchicon_*` tools via `ToolRegistry.Execute` with a test `Pool` + seeded tenant.
   - Same cross-contamination matrix as DB tests but through the tool surface.

4. **Frontend (`frontend/src/lib/category-store.test.tsx`, `frontend/src/components/CategoryFolder.test.tsx`, `frontend/src/api/categories.test.ts`):**
   - Mock the Connect client; assert `useListCategories(WORKER)` never renders workflow categories.
   - Component test: create/rename/delete/assign on each of the three pages (reuse existing Vitest setup — see `frontend/src/api/workItems.keys.test.ts` as a pattern for key isolation).
   - Mobile persistence: render the page, create a category, unmount the sheet, remount, assert the category persists (because the mock server still returns it).

**`make ci` gate:** `go test ./...` + `semgrep scan --config .orchicon/semgrep_orchicon.yml` scoped to changed files + frontend `vitest` + `tsc` + `eslint`. Only new findings matter (per `worker.md` baseline).

---

## 3. File map (likely touched)

| Area | File | Action |
|------|------|--------|
| Proto | `proto/orchicon/api/v1/category.proto` | **Create** |
| Proto | `proto/orchicon/api/v1/category_service.proto` | **Create** |
| Proto | `proto/orchicon/api/v1/worker_service.proto` | **Edit** — add categories/assignments to `ListWorkersResponse` (if Option A) |
| Proto | `proto/orchicon/api/v1/workflow_service.proto` | **Edit** — same |
| Proto | `proto/orchicon/api/v1/ask_orchicon_service.proto` | **Edit** — same for `ListConversationsResponse` |
| DB | `db/migrations/20260831*_categories.sql` | **Create** (Atlas, idempotent, RLS) + `*_down.sql` + `atlas.hcl` hash |
| DB | `db/schema.hcl` | **Edit** — add tables to declarative schema (if Atlas is declarative) |
| DB | `internal/db/category.go` | **Create** — `CategoryRow`, `AssignmentRow`, `CreateCategory`, `ListCategories`, `UpdateCategory`, `DeleteCategory`, `AssignToCategory`, `UnassignFromCategory`, `ReorderCategories` |
| DB | `internal/db/category_test.go` | **Create** |
| Service | `internal/category/service.go` | **Create** — RPC handlers (validation, tenant, audit) |
| Service | `internal/server/server.go` | **Edit** — register `CategoryService` + Connect handler |
| MCP | `internal/askorchicon/tool_categories.go` | **Create** — 7 tools |
| MCP | `internal/askorchicon/tools.go` | **Edit** — register tools |
| MCP | `internal/askorchicon/tools_test.go` | **Edit** — add category tool tests (or new file) |
| Frontend API | `frontend/src/api/clients.ts` | **Edit** — add `categoryClient` |
| Frontend API | `frontend/src/api/categories.ts` | **Create** — query/mutation hooks |
| Frontend lib | `frontend/src/lib/category-store.ts` | **Edit** — keep collapse, add migration helper, deprecate local category state |
| Frontend routes | `frontend/src/routes/workers.tsx` | **Edit** — use server categories |
| Frontend routes | `frontend/src/routes/workflows.tsx` | **Edit** — same |
| Frontend routes | `frontend/src/routes/ask-orchicon.tsx` | **Edit** — same |
| Frontend components | `frontend/src/components/CategoryFolder.tsx` | **Edit** — no logic change, props become server-driven |
| Frontend components | `frontend/src/components/CategoryDndContext.tsx` | **Edit** — `onAssign` becomes server mutation |
| Frontend components | `frontend/src/components/CreateCategoryDialog.tsx` | **Edit** — `onCreate` becomes server mutation |
| Generated | `api/gen/**` | **Regenerate** via `buf generate` |

---

## 4. Scalability, security, observability, operability

### Scale (10x)
- Per-tenant category count is bounded (tens, not thousands) — `sort_order` re-numbering is O(n) per reorder where n is per-set count, not global. Index on `(tenant_id, target_type, sort_order)` keeps list scans cheap.
- Assignments scale with entity count (workers ~ hundreds, workflows ~ hundreds, conversations ~ thousands per tenant). Index on `(tenant_id, entity_id)` makes `Unassign`/`Assign` point lookups fast; `ListAssignments` for a page's entity ids uses `WHERE entity_id = ANY($1)` in one query.
- No N+1: each `List*` enriches categories + assignments in one join per list call (same pattern as `ListWorkersWithActiveVersion`).

### Security
- Tenant isolation: `BeginTenantTx` + `set_config('app.tenant_id')` on every transaction; RLS is the backstop. Every accessor filters by `(tenant_id, target_type)`.
- Validation: name/description length, `target_type` enum, slug uniqueness. No raw SQL outside `internal/db` (AGENTS.md invariant #4).
- Audit: every mutation writes `audit_events` in the same tx (`category.created` etc.). Private storage failure (blocked `localStorage`) no longer loses data — server is the source of truth.
- No secrets in this feature; category names/descriptions are not sensitive but are still tenant-scoped.

### Observability
- Audit trail provides the "who did what" for category changes (actor, trace_id, before/after).
- Existing structured logging + OTel spans cover the new service; no new metrics needed beyond the audit events.
- Frontend: TanStack Query devtools show category cache keys (`["categories", targetType]`).

### Operability
- Atlas migration is idempotent (`IF NOT EXISTS` + `DO $$` for RLS policy) so re-running `make migrate` is safe.
- `ReorderCategories` is transactional — no partial reorder on crash.
- Downgrade path: dropping the two tables (`DROP TABLE category_assignments, categories`) restores the pre-feature state; localStorage migration is best-effort and non-destructive.

---

## 5. Alternatives explored (summary)

| Option | Verdict |
|--------|---------|
| Three separate tables | Rejected — duplicates schema/code 3x, contradicts requirement. |
| Category id on entity row (FK column) | Rejected — alters three unrelated tables, couples entity lifecycle to org layer. |
| Client-only fix (keep localStorage, fix Sheet) | Rejected — does not fix reload, multi-device, worker/MCP visibility, or tenant isolation. |
| Server categories but keep `category-store.ts` as write-through cache | Considered — but requirement says local storage degrades to a cache, not the source of truth; the chosen design does this via the one-time seeding, then stops reading it. |
| Separate service per target_type (`WorkerCategoryService`, etc.) | Rejected — multiplies proto/service/handler surface for identical logic; single `CategoryService` with `target_type` discriminator is simpler and enforces the "no crossover" invariant in one place. |

---

## 6. Risks & mitigations

| Risk | Mitigation |
|------|------------|
| Existing localStorage seed (`cat_software_dev`) duplicates on first server create | Slug/name uniqueness per `(tenant, target_type)` surfaces as `AlreadyExists`; seeder maps by name, so the duplicate is a no-op. |
| Concurrent `ReorderCategories` from two tabs | `FOR UPDATE` serializes; second caller gets `Aborted`, frontend retries after refetch. |
| Frontend waterfall (categories fetch then items fetch) | Enrich `List*` responses (ADR-002 Option A) so one fetch returns both; or prefetch categories in the route loader. |
| `entity_id` does not exist (stale assignment) | Assignment is organizational — allow orphan assignments; alternatively validate existence on `Assign` and lazily GC on `List` (filter to existing entity ids). |
| `make ci` semgrep drift | Scope semgrep to changed Go files only (`git diff --name-only origin/develop...HEAD | grep -E '\.go$'`). |

---

## 7. Review checklist (for Step 2: Design Approver)

- [ ] Does the design scale? What breaks at 10x? — See §4; category count is O(10s), assignments O(entity count), both indexed.
- [ ] Are we building the right thing? — Fixes the reported mobile bug by moving source of truth server-side; does not re-introduce localStorage as source of truth.
- [ ] Security, observability, operability considered? — §4; tenant isolation, audit, RLS, Atlas idempotency.
- [ ] Trade-offs documented? Alternatives explored? — §5.
- [ ] Is the design consistent with existing architecture? — Uses `internal/db` helpers, `BeginTenantTx`, `audit_events`, Connect-REST, TanStack Query, Atlas, same as workers/workflows/conversations.

---

## 8. Implementation sequence (for Step 3: Senior Software Engineer)

1. **Migration** — `db/migrations/*_categories.sql` + `schema.hcl` + `make migrate-hash`.
2. **DB layer** — `internal/db/category.go` + `category_test.go` (unit + cross-contamination).
3. **Proto** — `category.proto` + `category_service.proto`, edits to three List* protos, `buf generate`, commit `api/gen/**`.
4. **RPC service** — `internal/category/service.go`, wire into `internal/server/server.go`, handler tests.
5. **MCP tools** — `internal/askorchicon/tool_categories.go`, register in `tools.go`, tests.
6. **Frontend API** — `frontend/src/api/categories.ts`, `frontend/src/api/clients.ts`.
7. **Frontend routes** — replace `useCategoryPreferences` category/assignment bits with `useListCategories` + mutations; keep collapse local; wire `CategoryDndContext` + `CategoryFolder` + `CreateCategoryDialog` to server mutations.
8. **Migration helper** — best-effort seeder in `category-store.ts` (or new `useSeedCategories` hook), per-page `useEffect`.
9. **Tests** — add DB/RPC/MCP/frontend cross-contamination tests; run `make ci` (Go, semgrep, frontend build).

---

*End of architecture notes. Next step (Design Approver) should verify the ADRs, especially ADR-002 Option A vs B and the `UNIQUE (tenant_id, target_type, name)` vs permissive duplicate-name policy, before handing off to the Senior Software Engineer.*
