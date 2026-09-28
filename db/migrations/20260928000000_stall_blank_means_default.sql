-- Blank means DEFAULT: NULL = "use the built-in default", 0 = "disabled".
--
-- The operator: a never-configured tenant should have every stall field BLANK,
-- and blank must mean the built-in default — while 0 must mean disabled. That is
-- three states, and the point of this migration is to encode them unambiguously.
--
-- BEFORE this migration the stall columns were `NOT NULL DEFAULT 0` and the
-- resolution chain read 0 as "unset, so use the built-in default" — an
-- overloaded zero. Two things were wrong with it:
--
--   * "0 = disabled" was UNREACHABLE on most dimensions. Writing 0 meant
--     "unset", so the built-in default applied and the operator could not
--     actually switch a check off. Disabling had been bolted on as NEGATIVE =
--     disabled, which works but is unintuitive: nothing about -1 reads as "off",
--     and it is the kind of value an operator types by accident.
--
--   * A never-configured tenant and an operator who deliberately chose 0 were
--     INDISTINGUISHABLE, because both are the column default.
--
-- AFTER: NULL = blank = "use the built-in default"; 0 = disabled; negative is
-- rejected at the API boundary (settings.Service.validateStallSettings). This
-- mirrors the budget ladder, whose gate columns already distinguish NULL
-- (built-in default) from an explicit 0 (disabled) via *float64 — so the two
-- halves of the Settings page finally speak the same language.
--
-- DROP NOT NULL, not DROP COLUMN: the columns stay, and every read path already
-- passes them through a nullable Go field, so no data has to move.
--
-- TWO DATA NORMALISATIONS, both preserving the intent already on disk:
--
--   * 0 -> NULL. Under the old semantics 0 meant "unset"; NULL is the new
--     "unset". Without this every tenant that has ever been created would read
--     back as "disabled" and silently LOSE stall detection — the exact opposite
--     of the intent, and the worst possible failure mode for a safety check.
--
--   * negative -> 0. A negative was the old spelling of "disabled"; 0 is the new
--     one. Mapping them keeps every check that is disabled today disabled.
--     Without this a stored -1 would fail the new validation and block the next
--     save of an unrelated setting.

ALTER TABLE tenant_settings ALTER COLUMN stall_no_progress_window_seconds DROP NOT NULL;
ALTER TABLE tenant_settings ALTER COLUMN stall_no_progress_window_seconds DROP DEFAULT;
ALTER TABLE tenant_settings ALTER COLUMN stall_no_file_diff_window_seconds DROP NOT NULL;
ALTER TABLE tenant_settings ALTER COLUMN stall_no_file_diff_window_seconds DROP DEFAULT;
ALTER TABLE tenant_settings ALTER COLUMN stall_text_loop_window_seconds DROP NOT NULL;
ALTER TABLE tenant_settings ALTER COLUMN stall_text_loop_window_seconds DROP DEFAULT;
ALTER TABLE tenant_settings ALTER COLUMN stall_repetition_count DROP NOT NULL;
ALTER TABLE tenant_settings ALTER COLUMN stall_repetition_count DROP DEFAULT;
ALTER TABLE tenant_settings ALTER COLUMN stall_repetition_window_seconds DROP NOT NULL;
ALTER TABLE tenant_settings ALTER COLUMN stall_repetition_window_seconds DROP DEFAULT;
ALTER TABLE tenant_settings ALTER COLUMN stall_nudge_max DROP NOT NULL;
ALTER TABLE tenant_settings ALTER COLUMN stall_nudge_max DROP DEFAULT;
ALTER TABLE tenant_settings ALTER COLUMN stall_nudge_reply_window_seconds DROP NOT NULL;
ALTER TABLE tenant_settings ALTER COLUMN stall_nudge_reply_window_seconds DROP DEFAULT;
ALTER TABLE tenant_settings ALTER COLUMN stall_nudge_cooldown_seconds DROP NOT NULL;
ALTER TABLE tenant_settings ALTER COLUMN stall_nudge_cooldown_seconds DROP DEFAULT;
ALTER TABLE tenant_settings ALTER COLUMN stall_tool_hang_seconds DROP NOT NULL;
ALTER TABLE tenant_settings ALTER COLUMN stall_tool_hang_seconds DROP DEFAULT;

-- Old "unset" (0) becomes the new "unset" (NULL).
UPDATE tenant_settings SET stall_no_progress_window_seconds = NULL WHERE stall_no_progress_window_seconds = 0;
UPDATE tenant_settings SET stall_no_file_diff_window_seconds = NULL WHERE stall_no_file_diff_window_seconds = 0;
UPDATE tenant_settings SET stall_text_loop_window_seconds = NULL WHERE stall_text_loop_window_seconds = 0;
UPDATE tenant_settings SET stall_repetition_count = NULL WHERE stall_repetition_count = 0;
UPDATE tenant_settings SET stall_repetition_window_seconds = NULL WHERE stall_repetition_window_seconds = 0;
UPDATE tenant_settings SET stall_nudge_max = NULL WHERE stall_nudge_max = 0;
UPDATE tenant_settings SET stall_nudge_reply_window_seconds = NULL WHERE stall_nudge_reply_window_seconds = 0;
UPDATE tenant_settings SET stall_nudge_cooldown_seconds = NULL WHERE stall_nudge_cooldown_seconds = 0;
UPDATE tenant_settings SET stall_tool_hang_seconds = NULL WHERE stall_tool_hang_seconds = 0;

-- Old "disabled" (negative) becomes the new "disabled" (0).
UPDATE tenant_settings SET stall_no_progress_window_seconds = 0 WHERE stall_no_progress_window_seconds < 0;
UPDATE tenant_settings SET stall_no_file_diff_window_seconds = 0 WHERE stall_no_file_diff_window_seconds < 0;
UPDATE tenant_settings SET stall_text_loop_window_seconds = 0 WHERE stall_text_loop_window_seconds < 0;
UPDATE tenant_settings SET stall_repetition_count = 0 WHERE stall_repetition_count < 0;
UPDATE tenant_settings SET stall_repetition_window_seconds = 0 WHERE stall_repetition_window_seconds < 0;
UPDATE tenant_settings SET stall_nudge_max = 0 WHERE stall_nudge_max < 0;
UPDATE tenant_settings SET stall_nudge_reply_window_seconds = 0 WHERE stall_nudge_reply_window_seconds < 0;
UPDATE tenant_settings SET stall_nudge_cooldown_seconds = 0 WHERE stall_nudge_cooldown_seconds < 0;
UPDATE tenant_settings SET stall_tool_hang_seconds = 0 WHERE stall_tool_hang_seconds < 0;
