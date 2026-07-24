-- recovery_policy_ref is obsolete — recovery is now configured per-step
-- via the step config's "recovery" block (strategy, max_attempts).
ALTER TABLE workflow_versions DROP COLUMN IF EXISTS recovery_policy_ref;
