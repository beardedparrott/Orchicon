-- Log management settings: control the serve process's rotating file
-- logger (size ceiling, time roll, retention). Each column is empty/zero
-- until the operator sets it in Settings → Defaults; the serve process
-- falls back to ORCHICON_LOG_* env vars, then to built-in defaults.
-- Additive-only and safe for rollback.
ALTER TABLE tenant_settings ADD COLUMN IF NOT EXISTS log_directory text NOT NULL DEFAULT '';
ALTER TABLE tenant_settings ADD COLUMN IF NOT EXISTS log_max_size_mb bigint NOT NULL DEFAULT 0;
ALTER TABLE tenant_settings ADD COLUMN IF NOT EXISTS log_roll_interval_hours bigint NOT NULL DEFAULT 0;
ALTER TABLE tenant_settings ADD COLUMN IF NOT EXISTS log_retention_days integer NOT NULL DEFAULT 0;
ALTER TABLE tenant_settings ADD COLUMN IF NOT EXISTS log_max_files integer NOT NULL DEFAULT 0;
