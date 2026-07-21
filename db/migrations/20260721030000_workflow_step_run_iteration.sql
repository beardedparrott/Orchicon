-- Add iteration tracking to workflow_step_runs for loop decision re-entry
-- (docs/11 §3.4). iteration tracks the re-entry count (0 for first run);
-- superseded_by links to the step run id that superseded this one, preserving
-- the audit trail of previous iterations.
-- The existing tenant_isolation RLS policy on workflow_step_runs covers
-- both new columns (row-level, tenant_id is already on every row).
ALTER TABLE workflow_step_runs
  ADD COLUMN IF NOT EXISTS iteration     INT  NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS superseded_by TEXT;

COMMENT ON COLUMN workflow_step_runs.iteration
  IS 'Re-entry count for loop decision steps (docs/11 §3.4). 0 for first dispatch.';
COMMENT ON COLUMN workflow_step_runs.superseded_by
  IS 'Step run id that superseded this one. Non-null for superseded (archived) iterations.';
