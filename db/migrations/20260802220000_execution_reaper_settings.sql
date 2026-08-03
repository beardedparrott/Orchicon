-- Add execution-liveness reaper tuning to tenant_settings.
-- Additive-only and safe for rollback.
ALTER TABLE tenant_settings ADD COLUMN IF NOT EXISTS execution_reap_grace_seconds bigint NOT NULL DEFAULT 60;
ALTER TABLE tenant_settings ADD COLUMN IF NOT EXISTS execution_reap_consecutive_failures integer NOT NULL DEFAULT 3;
