-- Reverse 20260828000000_usage_cache_tokens.sql (rollback between binary
-- versions). Drops the canonical usage-sample token buckets. Existing rows
-- are unaffected — the columns are dropped, not their data.
ALTER TABLE usage_records DROP COLUMN IF EXISTS cache_read_tokens;
ALTER TABLE usage_records DROP COLUMN IF EXISTS cache_write_tokens;
ALTER TABLE usage_records DROP COLUMN IF EXISTS reasoning_tokens;
