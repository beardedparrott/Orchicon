-- Add the tenant-level default wall-clock timeout to tenant_settings.
-- Applied when a worker's budget_overrides do not explicitly set
-- wall_clock_seconds. Additive-only and safe for rollback.
ALTER TABLE tenant_settings ADD COLUMN IF NOT EXISTS stall_wall_clock_seconds bigint NOT NULL DEFAULT 0;
