-- Sequential multi-workflow runs: sibling ordering.
--
-- sort_order is the sibling order within (parent_id): the sequence engine's
-- cursor. Nullable and only meaningful within a parent (top-level = parent_id
-- IS NULL group). Backfilled by created_at for existing rows; from then on it
-- is changed ONLY by ReorderWorkItems (explicit drag) — display sort
-- (ListWorkItems sort_by) never mutates it. Same table -> no new RLS policy
-- needed (row-level RLS already exists on work_items).
ALTER TABLE "work_items" ADD COLUMN IF NOT EXISTS "sort_order" double precision;

-- Backfill by created_at, scoped per parent (top-level = parent_id IS NULL).
WITH ranked AS (
  SELECT id,
         row_number() OVER (PARTITION BY parent_id ORDER BY created_at, id) AS rn
  FROM work_items
)
UPDATE work_items w SET sort_order = r.rn
FROM ranked r WHERE w.id = r.id AND w.sort_order IS NULL;

CREATE INDEX IF NOT EXISTS idx_work_items_project_parent_sort
  ON work_items (project_id, parent_id, sort_order);

COMMENT ON COLUMN work_items.sort_order IS 'Sibling order within (parent_id). Nullable; backfilled by created_at. Only changed by ReorderWorkItems (drag), never by display sort.';
