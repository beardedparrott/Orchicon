ALTER TABLE projects
  DROP COLUMN IF EXISTS max_concurrent_runs;

ALTER TABLE tenant_settings
  DROP COLUMN IF EXISTS max_concurrent_runs;

DROP INDEX IF EXISTS worker_executions_project_status_idx;
