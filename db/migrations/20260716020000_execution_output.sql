ALTER TABLE worker_executions
  ADD COLUMN output text NOT NULL DEFAULT '';

-- The output column is populated by the TaskReconciler on OnResult so the
-- model's text output survives page navigation (docs/10 §11).
