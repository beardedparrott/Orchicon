-- In-flight tool-hang watchdog setting (core-tool-suite work item, D6).
-- 0 = unset (env/code default 180s); negative = disabled.
ALTER TABLE tenant_settings ADD COLUMN IF NOT EXISTS stall_tool_hang_seconds BIGINT NOT NULL DEFAULT 0;
