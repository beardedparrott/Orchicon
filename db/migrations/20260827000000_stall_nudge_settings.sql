-- Add nudge knobs (advisory-stall escalation) to tenant_settings.
-- Nudge-first routing: an advisory stall (text_loop / repetition /
-- no_file_progress) nudges the live session first, and only escalates to a
-- fatal kill + recovery after the nudge budget is spent. These three
-- columns tune that budget per-tenant. Additive-only and safe for rollback.
ALTER TABLE tenant_settings ADD COLUMN IF NOT EXISTS stall_nudge_max integer NOT NULL DEFAULT 0;
ALTER TABLE tenant_settings ADD COLUMN IF NOT EXISTS stall_nudge_reply_window_seconds bigint NOT NULL DEFAULT 0;
ALTER TABLE tenant_settings ADD COLUMN IF NOT EXISTS stall_nudge_cooldown_seconds bigint NOT NULL DEFAULT 0;
