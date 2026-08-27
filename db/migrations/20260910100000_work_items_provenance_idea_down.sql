-- Down migration for 20260910100000_work_items_provenance_idea.sql
-- (The one-time flat-shape data normalization is intentionally one-way; the
-- platform cannot reconstruct a prior kind/parent for a recurring row.)

DROP INDEX IF EXISTS "idx_workflow_runs_work_item_id";
DROP INDEX IF EXISTS "idx_work_items_status_idea";

ALTER TABLE "work_items"
  DROP COLUMN IF EXISTS "spawned_by_work_item_id",
  DROP COLUMN IF EXISTS "spawned_by_run_id";
