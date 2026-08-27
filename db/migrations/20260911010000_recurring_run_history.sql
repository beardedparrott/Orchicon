-- Per-fire run-history ledger for recurring work items (feature 4.2).
--
-- A fire can fail BEFORE a workflow run exists (dispatch error, unresolvable
-- runtime image, missing/deprecated workflow version, no bound workflow),
-- so workflow_runs alone cannot represent history. The ledger records the
-- fire fact (status = 'fired' | 'failed') + an optional reference to the
-- run it produced; started/ended timestamps and execution ids/outputs are
-- NOT duplicated here — they live in the run graph (workflow_runs,
-- worker_executions) and are joined at read time so a retried/recovered
-- run never drifts from the ledger.
CREATE TABLE IF NOT EXISTS "recurring_run_history" (
  "id"              TEXT NOT NULL PRIMARY KEY,
  "tenant_id"       TEXT NOT NULL,
  "work_item_id"    TEXT NOT NULL,
  "fire_at"         TIMESTAMPTZ NOT NULL DEFAULT now(),
  "status"          TEXT NOT NULL,          -- 'fired' | 'failed' (fire dispatch outcome)
  "workflow_run_id" TEXT NULL,              -- set after a successful dispatch; NULL when the fire failed before a run
  "error"           TEXT NULL               -- set on a failed fire (dispatch error)
);

-- Ordered per-fire history for the item detail view / API, newest first.
CREATE INDEX IF NOT EXISTS idx_recurring_run_history_item
  ON "recurring_run_history" ("tenant_id", "work_item_id", "fire_at" DESC);

-- Tenant isolation, same policy as work_items (docs/09 §8.5).
ALTER TABLE "recurring_run_history" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "recurring_run_history" FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON "recurring_run_history"
  FOR ALL
  USING ("tenant_id" = current_setting('app.tenant_id', true));

COMMENT ON TABLE recurring_run_history
  IS 'Per-fire ledger for recurring work items. status = fired | failed (fire dispatch outcome); workflow_run_id links to the run graph for start/end + execution outputs.';
