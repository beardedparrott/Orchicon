-- Add scheduled start columns to work_items for template-based bound runs
-- (docs/11 §5.1). scheduled_start_at: future wall-clock time for the bound
-- run to fire; NULL = start immediately when auto_start_workflow is true.
-- auto_start_workflow: true (default) starts the run immediately on save;
-- false defers to explicit user action.
-- The existing tenant_isolation RLS policy on work_items covers both new
-- columns (row-level, tenant_id is already on every row).
ALTER TABLE work_items
  ADD COLUMN IF NOT EXISTS scheduled_start_at    TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS auto_start_workflow   BOOLEAN NOT NULL DEFAULT TRUE;

COMMENT ON COLUMN work_items.scheduled_start_at
  IS 'Scheduled start time for a bound workflow run (docs/11 §5.1). NULL = immediate.';
COMMENT ON COLUMN work_items.auto_start_workflow
  IS 'If true, automatically start the bound workflow on save. Set false to defer (docs/11 §2.1).';
