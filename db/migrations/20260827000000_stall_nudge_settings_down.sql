-- Reverse 20260827000000_stall_nudge_settings.sql (rollback between binary
-- versions). Drops the nudge knob columns.
ALTER TABLE tenant_settings DROP COLUMN IF EXISTS stall_nudge_max;
ALTER TABLE tenant_settings DROP COLUMN IF EXISTS stall_nudge_reply_window_seconds;
ALTER TABLE tenant_settings DROP COLUMN IF EXISTS stall_nudge_cooldown_seconds;
