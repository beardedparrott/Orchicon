-- Feature 4.1: provenance + idea state for work items (flat recurring type).
-- Additive only: ordinary non-recurring rows are untouched, default query
-- behavior is unchanged — backward compatible by construction.

ALTER TABLE "work_items"
  ADD COLUMN IF NOT EXISTS "spawned_by_work_item_id" TEXT NULL,
  ADD COLUMN IF NOT EXISTS "spawned_by_run_id"       TEXT NULL;

COMMENT ON COLUMN work_items.spawned_by_work_item_id
  IS 'ID of the recurring work item whose fire created this item (flat-recurring provenance). NULL for ordinary items.';
COMMENT ON COLUMN work_items.spawned_by_run_id
  IS 'ID of the workflow run (fire) that spawned this item. NULL for ordinary items.';

-- Idea Cloud query: idea-state items are top-level flat items created by a
-- recurring fire. A tenant-scoped partial index keeps the scan small.
CREATE INDEX IF NOT EXISTS idx_work_items_status_idea
  ON "work_items" ("tenant_id", "status")
  WHERE "status" = 'idea';

-- Run history: each fire's run is keyed by the recurring work item row.
-- Reuses workflow_runs.work_item_id (FK ON DELETE SET NULL) for per-item
-- run queries — no new table.
CREATE INDEX IF NOT EXISTS idx_workflow_runs_work_item_id
  ON "workflow_runs" ("work_item_id");

-- One-time flat-shape normalization: rows already marked recurring before D1
-- (flat type) landed must conform to the flat shape — kind=task, no parent.
-- Only touches rows whose recurring_schedule is set and that violate the
-- flat shape; ordinary non-recurring rows and compliant rows are untouched.
UPDATE "work_items"
  SET "kind" = 'task', "parent_id" = NULL
  WHERE "recurring_schedule" IS NOT NULL
    AND ("kind" <> 'task' OR "parent_id" IS NOT NULL);
