-- Add backup configuration columns to tenant_settings.
-- These columns are additive-only and safe for rollback.
ALTER TABLE tenant_settings ADD COLUMN IF NOT EXISTS backup_schedule text NOT NULL DEFAULT '';
ALTER TABLE tenant_settings ADD COLUMN IF NOT EXISTS backup_retention_days integer NOT NULL DEFAULT 0;
ALTER TABLE tenant_settings ADD COLUMN IF NOT EXISTS backup_directory text NOT NULL DEFAULT '';
