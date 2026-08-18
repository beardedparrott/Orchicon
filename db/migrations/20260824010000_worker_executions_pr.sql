-- Worker executions: PR surface mirrored from the parent workflow run.
--
-- The run-level PR URL/state (parsed from workflow_runs.run_context at read
-- time) is the source of truth; the execution-level columns are written at
-- dispatch so the executions list/detail can link out to the run's PR
-- without a join. Nullable: rows predate PRs (backfill null); a null value
-- means no PR info is known and the UI falls back to the deterministic
-- `pull/new/{branch}` link derived from the project's repo_slug.
--
-- Additive and forward-only; RLS untouched (existing tenant_isolation
-- policy already scopes the row).
ALTER TABLE worker_executions
  ADD COLUMN IF NOT EXISTS pr_url text,
  ADD COLUMN IF NOT EXISTS pr_state text;

COMMENT ON COLUMN worker_executions.pr_url IS 'PR URL for the run''s branch, mirrored from the parent workflow run at dispatch.';
COMMENT ON COLUMN worker_executions.pr_state IS 'PR state (open/merged/draft/none), mirrored from the parent workflow run at dispatch.';
