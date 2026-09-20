-- Reverse of 20260920000000_ask_usage_adapter_kind (best-effort).
--
-- The forward migration added usage_records.adapter_kind + session_id.
-- This drops the columns. Per-row values are lost on rollback; the
-- down migration is a dev-rollback convenience, not a data guarantee;
-- migrations are forward-only and this file exists to satisfy the
-- paired _down.sql convention.
ALTER TABLE usage_records DROP COLUMN IF EXISTS adapter_kind;
ALTER TABLE usage_records DROP COLUMN IF EXISTS session_id;
