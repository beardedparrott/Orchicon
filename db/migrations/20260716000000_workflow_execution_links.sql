-- Track the workflow run and step that created a WorkerExecution so the
-- UI can show which workflow/run/step each execution belongs to.
--
-- The work_items table also gets these columns so the TaskReconciler can
-- propagate them to the execution at dispatch time.

ALTER TABLE "work_items"
  ADD COLUMN "workflow_run_id" text NOT NULL DEFAULT '',
  ADD COLUMN "workflow_step_id" text NOT NULL DEFAULT '';

COMMENT ON COLUMN "work_items"."workflow_run_id"
  IS 'The workflow run that dispatched this work item. Set by WorkflowReconciler.';
COMMENT ON COLUMN "work_items"."workflow_step_id"
  IS 'The workflow step run that dispatched this work item. Set by WorkflowReconciler.';

ALTER TABLE "worker_executions"
  ADD COLUMN "workflow_run_id" text NOT NULL DEFAULT '',
  ADD COLUMN "workflow_step_id" text NOT NULL DEFAULT '';

COMMENT ON COLUMN "worker_executions"."workflow_run_id"
  IS 'The workflow run that created this execution. Propagated from the parent work item.';
COMMENT ON COLUMN "worker_executions"."workflow_step_id"
  IS 'The workflow step run that created this execution. Propagated from the parent work item.';
