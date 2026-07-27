-- Smarter backfill for worker_executions.workflow_run_id.
-- The previous backfill (20260731000013) copied work_items.workflow_run_id
-- directly, but that column is overwritten each time the WorkflowReconciler
-- re-dispatches a work item — it always points to the LATEST run. For
-- template workflows with many runs, all old executions got the same
-- latest-run ID and collapsed into one cost row.
--
-- This migration uses timing: for each execution, find the workflow run
-- whose started_at is the latest timestamp ≤ the execution's created_at.
-- This correctly recovers run attribution for historical data.

UPDATE worker_executions we
SET workflow_run_id = sq.best_run_id
FROM (
  SELECT DISTINCT ON (we2.id)
    we2.id AS exec_id,
    wr2.id AS best_run_id
  FROM worker_executions we2
  JOIN work_items wi2 ON wi2.id = we2.task_id AND wi2.tenant_id = we2.tenant_id
  -- Get the workflow_id from the latest run the work item was dispatched in
  -- (even though this is the latest-run value, it correctly identifies the workflow)
  JOIN workflow_runs wr_latest ON wr_latest.id = wi2.workflow_run_id AND wr_latest.tenant_id = we2.tenant_id
  -- Find all runs for the same workflow
  JOIN workflow_runs wr2 ON wr2.workflow_id = wr_latest.workflow_id AND wr2.tenant_id = we2.tenant_id
  WHERE we2.workflow_run_id IS DISTINCT FROM wr2.id
    AND COALESCE(wr2.started_at, wr2.created_at) <= we2.created_at
    AND (wr2.ended_at IS NULL OR we2.created_at <= wr2.ended_at + interval '5 minutes')
  ORDER BY we2.id, COALESCE(wr2.started_at, wr2.created_at) DESC
) sq
WHERE we.id = sq.exec_id;
