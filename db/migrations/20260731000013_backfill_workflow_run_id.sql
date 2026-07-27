-- Backfill worker_executions.workflow_run_id from work_items for rows
-- where it was not populated at creation time (pre-migration gap).
-- For work items re-dispatched across multiple runs, this sets ALL
-- executions to the LATEST run ID — imperfect, but recovers runs that
-- would otherwise be invisible in the cost explorer (or pan&14).

UPDATE worker_executions we
SET workflow_run_id = wi.workflow_run_id
FROM work_items wi
WHERE we.task_id = wi.id
  AND we.workflow_run_id = ''
  AND wi.workflow_run_id <> '';
