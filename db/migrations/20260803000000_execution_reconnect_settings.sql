-- Add execution transport-resilience (reconnect) tuning to tenant_settings.
-- Additive-only and safe for rollback.
ALTER TABLE tenant_settings ADD COLUMN IF NOT EXISTS execution_reconnect_attempts integer NOT NULL DEFAULT 3;
ALTER TABLE tenant_settings ADD COLUMN IF NOT EXISTS execution_reconnect_grace_seconds bigint NOT NULL DEFAULT 60;
