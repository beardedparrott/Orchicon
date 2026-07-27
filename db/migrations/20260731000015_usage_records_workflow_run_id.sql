-- Store workflow_run_id on usage_records so cost attribution survives
-- worker_executions deletion. Populated at record time from the execution's
-- workflow_run_id (set at creation, never changes). This is the immutable,
-- durable link between cost and workflow run.

ALTER TABLE usage_records
  ADD COLUMN IF NOT EXISTS "workflow_run_id" text NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS usage_records_workflow_run_idx
  ON usage_records ("tenant_id", "workflow_run_id", "occurred_at");
