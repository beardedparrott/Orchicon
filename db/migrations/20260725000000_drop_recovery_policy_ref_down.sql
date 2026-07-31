-- Reverse: re-add the recovery_policy_ref column.
ALTER TABLE workflow_versions ADD COLUMN IF NOT EXISTS recovery_policy_ref text NOT NULL DEFAULT '';
