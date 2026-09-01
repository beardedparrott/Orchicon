-- Roll back the in-flight tool-hang watchdog column.
ALTER TABLE tenant_settings DROP COLUMN IF EXISTS stall_tool_hang_seconds;
