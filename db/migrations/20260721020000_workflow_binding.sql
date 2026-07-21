-- Add work-item binding columns to workflow_runs for template-based runs
-- (docs/11 §5.1, §7). work_item_id links the run to the bound work item;
-- bound_worker_ref is reserved for future use (PR-A leaves it null).
-- The existing tenant_isolation RLS policy on workflow_runs covers both
-- new columns (row-level, tenant_id is already on every row).
ALTER TABLE workflow_runs
  ADD COLUMN IF NOT EXISTS work_item_id   TEXT REFERENCES work_items(id),
  ADD COLUMN IF NOT EXISTS bound_worker_ref JSONB;

COMMENT ON COLUMN workflow_runs.work_item_id
  IS 'Work item this bound run operates on (docs/11 §2.1). NULL for one-shot runs.';
COMMENT ON COLUMN workflow_runs.bound_worker_ref
  IS 'Reserved: explicit worker override for bound runs (docs/11 §2.1). NULL in PR-A.';
