-- Down migration for 20260731000018_backup_settings.sql

ALTER TABLE tenant_settings DROP COLUMN IF EXISTS backup_schedule;
ALTER TABLE tenant_settings DROP COLUMN IF EXISTS backup_retention_days;
ALTER TABLE tenant_settings DROP COLUMN IF EXISTS backup_directory;
