-- Dev-rollback convention only (AGENTS.md invariant #9: migrations are
-- forward-only). Drops the constraint with the column, in that order.
ALTER TABLE work_items DROP CONSTRAINT IF EXISTS work_items_execution_mode_check;
ALTER TABLE work_items DROP COLUMN IF EXISTS execution_mode;
