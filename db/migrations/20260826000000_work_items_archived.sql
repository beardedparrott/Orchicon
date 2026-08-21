-- Work-item archiving: hide an item from all normal views + restore.
--
-- archived_at (NULL = active) is the default-view gate: every active
-- work-item read filters `archived_at IS NULL`, and the dedicated archive
-- view opts in via ListWorkItems include_archived (filters
-- `archived_at IS NOT NULL`). archived_from_status preserves the prior
-- terminal status so RestoreWorkItem can return the item to the terminal
-- state it was archived from (not pending). Both are nullable; archiving is
-- only allowed from a terminal status and only when the item has no
-- children, so no backfill is needed.
ALTER TABLE "work_items" ADD COLUMN IF NOT EXISTS "archived_at" timestamptz;
ALTER TABLE "work_items" ADD COLUMN IF NOT EXISTS "archived_from_status" text;

CREATE INDEX IF NOT EXISTS idx_work_items_archived_at
  ON work_items (archived_at);

COMMENT ON COLUMN work_items.archived_at IS 'Set when the work item is archived (NULL = active). Drives the default archived-at-IS-NULL filter on every active work-item read.';
COMMENT ON COLUMN work_items.archived_from_status IS 'The terminal status the item had when archived; RestoreWorkItem returns the item to this status. NULL = never archived.';
