-- Reverses 20260928000000_stall_blank_means_default.sql.
--
-- NOT A PERFECT INVERSE, and deliberately so: the down path cannot tell a NULL
-- that was always NULL from a NULL this migration created out of a 0, so every
-- blank column comes back as 0. Under the OLD semantics 0 meant "unset, use the
-- default", so this restores the previous BEHAVIOUR faithfully — including for
-- the one case that differs, a dimension this migration explicitly disabled
-- (0 -> NULL going up comes back as 0, which the old code reads as "unset", so a
-- disabled check re-enables on rollback). That is the safe direction: the old
-- schema's 0 carried no way to express "disabled" at all, so there is nothing to
-- restore it to.

-- COALESCE first: SET NOT NULL cannot apply while any NULL remains.
UPDATE tenant_settings SET stall_no_progress_window_seconds = COALESCE(stall_no_progress_window_seconds, 0);
UPDATE tenant_settings SET stall_no_file_diff_window_seconds = COALESCE(stall_no_file_diff_window_seconds, 0);
UPDATE tenant_settings SET stall_text_loop_window_seconds = COALESCE(stall_text_loop_window_seconds, 0);
UPDATE tenant_settings SET stall_repetition_count = COALESCE(stall_repetition_count, 0);
UPDATE tenant_settings SET stall_repetition_window_seconds = COALESCE(stall_repetition_window_seconds, 0);
UPDATE tenant_settings SET stall_nudge_max = COALESCE(stall_nudge_max, 0);
UPDATE tenant_settings SET stall_nudge_reply_window_seconds = COALESCE(stall_nudge_reply_window_seconds, 0);
UPDATE tenant_settings SET stall_nudge_cooldown_seconds = COALESCE(stall_nudge_cooldown_seconds, 0);
UPDATE tenant_settings SET stall_tool_hang_seconds = COALESCE(stall_tool_hang_seconds, 0);

ALTER TABLE tenant_settings ALTER COLUMN stall_no_progress_window_seconds SET DEFAULT 0;
ALTER TABLE tenant_settings ALTER COLUMN stall_no_progress_window_seconds SET NOT NULL;
ALTER TABLE tenant_settings ALTER COLUMN stall_no_file_diff_window_seconds SET DEFAULT 0;
ALTER TABLE tenant_settings ALTER COLUMN stall_no_file_diff_window_seconds SET NOT NULL;
ALTER TABLE tenant_settings ALTER COLUMN stall_text_loop_window_seconds SET DEFAULT 0;
ALTER TABLE tenant_settings ALTER COLUMN stall_text_loop_window_seconds SET NOT NULL;
ALTER TABLE tenant_settings ALTER COLUMN stall_repetition_count SET DEFAULT 0;
ALTER TABLE tenant_settings ALTER COLUMN stall_repetition_count SET NOT NULL;
ALTER TABLE tenant_settings ALTER COLUMN stall_repetition_window_seconds SET DEFAULT 0;
ALTER TABLE tenant_settings ALTER COLUMN stall_repetition_window_seconds SET NOT NULL;
ALTER TABLE tenant_settings ALTER COLUMN stall_nudge_max SET DEFAULT 0;
ALTER TABLE tenant_settings ALTER COLUMN stall_nudge_max SET NOT NULL;
ALTER TABLE tenant_settings ALTER COLUMN stall_nudge_reply_window_seconds SET DEFAULT 0;
ALTER TABLE tenant_settings ALTER COLUMN stall_nudge_reply_window_seconds SET NOT NULL;
ALTER TABLE tenant_settings ALTER COLUMN stall_nudge_cooldown_seconds SET DEFAULT 0;
ALTER TABLE tenant_settings ALTER COLUMN stall_nudge_cooldown_seconds SET NOT NULL;
ALTER TABLE tenant_settings ALTER COLUMN stall_tool_hang_seconds SET DEFAULT 0;
ALTER TABLE tenant_settings ALTER COLUMN stall_tool_hang_seconds SET NOT NULL;
