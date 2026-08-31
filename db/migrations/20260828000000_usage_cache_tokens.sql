-- Add canonical usage-sample token buckets to usage_records (docs
-- canonical-usage-sample-contract §5). The gateway now records a four-bucket
-- token breakdown (prompt / cache read / cache write / completion) plus an
-- optional reasoning sub-bucket, so cache and reasoning spend survives to the
-- row and wire instead of being thrown away. Additive-only and safe: the
-- columns default to 0 so existing rows stay valid (zero-fill, no data
-- rewrite) and reasoning is NEVER added to total_tokens (it is a sub-bucket
-- of completion, and providers that report it separately bill it as output).
ALTER TABLE usage_records ADD COLUMN IF NOT EXISTS cache_read_tokens bigint NOT NULL DEFAULT 0;
ALTER TABLE usage_records ADD COLUMN IF NOT EXISTS cache_write_tokens bigint NOT NULL DEFAULT 0;
ALTER TABLE usage_records ADD COLUMN IF NOT EXISTS reasoning_tokens bigint NOT NULL DEFAULT 0;
