-- PR C (recovery as work items): the recovery execution carries an
-- explicit strategy that the engine routes on. This decouples the
-- strategy from the fixed 6-step default flow (docs/06 §3) so
-- workflows can pick how a failure is recovered.
--
-- The 5 strategies:
--   summarize_restart  — capture → summarize → preserve → review → plan →
--                        resume (the default; current behavior).
--   stop               — abandon the workflow cleanly. Mark the run
--                        failed/aborted, no retry, no resumption.
--   human_escalation   — block the recovery until a human approves
--                        (L3 — docs/06 §7).
--   retry_n            — requeue the task immediately with the same
--                        input; defer all capture/summarize until
--                        budgets are exhausted (no bounded auto-relax).
--
-- DEFAULT 'summarize_restart' preserves prior behavior for existing
-- rows (which had no strategy column). New rows are written by the
-- recovery engine from the work_items.kind that triggered the
-- recovery (PR C — work items are first-class recovery carriers).

ALTER TABLE "recovery_executions"
  ADD COLUMN "strategy" text NOT NULL DEFAULT 'summarize_restart';

COMMENT ON COLUMN "recovery_executions"."strategy"
  IS 'Recovery strategy routed on by the engine (PR C). summarize_restart | stop | human_escalation | retry_n';
