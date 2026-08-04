-- Add the tenant-level default budget JSON to tenant_settings.
-- These are the default budgets applied to executions whose worker does not
-- set its own budget_overrides for a field. Recognized keys: tokens,
-- cost_usd, wall_clock_seconds, tool_call_count. A worker's explicit
-- budget_overrides overrides these per-field. Empty ({}) = use built-in
-- defaults. Additive-only and safe for rollback.
ALTER TABLE tenant_settings ADD COLUMN IF NOT EXISTS default_budget_overrides jsonb NOT NULL DEFAULT '{}'::jsonb;

-- Drop the scoped-in wall-clock column that preceded this consolidated budget
-- column (never shipped in a release; superseded by default_budget_overrides).
ALTER TABLE tenant_settings DROP COLUMN IF EXISTS stall_wall_clock_seconds;
