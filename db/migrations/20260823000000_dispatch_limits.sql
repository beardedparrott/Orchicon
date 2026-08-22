-- Per-project / per-tenant max-concurrent-runs (concurrency guards).
--
-- Without a cap, parallelism can overwhelm a single machine: the
-- ORCHICON_DISPATCH_CONCURRENCY bound limits only the in-pass fan-out of
-- one scan tick, not how many executions across all projects may be
-- in-flight at once. These two settings cap concurrently RUNNING
-- executions at dispatch time:
--
--   tenant_settings.max_concurrent_runs: a tenant-wide ceiling.
--   projects.max_concurrent_runs:        a per-project override.
--
-- The effective limit for a project is min(tenant, project), where 0 on
-- either side means "no additional restriction from that side" (i.e. the
-- other side, or the global default, applies). A project whose effective
-- limit is 1 serializes its runs.
--
-- Non-repo projects (the in-place fallback, where a run executes in the
-- shared project_dir) are serialized by default: their in-place limit is
-- 1 unless the project explicitly sets max_concurrent_runs > 1 AND the
-- tenant permits it. That explicit setting is the "opt-in" that accepts
-- the risk of concurrent in-place execution.
--
-- Additive and forward-only; RLS untouched. The `worker_executions`
-- index accelerates the per-project active-execution count the
-- TaskReconciler's admission gate runs on every dispatch.
ALTER TABLE projects
  ADD COLUMN IF NOT EXISTS max_concurrent_runs integer NOT NULL DEFAULT 0;

ALTER TABLE tenant_settings
  ADD COLUMN IF NOT EXISTS max_concurrent_runs integer NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS worker_executions_project_status_idx
  ON worker_executions (project_id, status);
