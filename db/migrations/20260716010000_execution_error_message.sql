-- Add error_message column to worker_executions for human-readable failure
-- reasons (docs/02 §2.7). Populated by the TaskReconciler when an execution
-- transitions to a terminal failure state. Empty string = no error.
ALTER TABLE "worker_executions" ADD COLUMN "error_message" text NOT NULL DEFAULT '';
