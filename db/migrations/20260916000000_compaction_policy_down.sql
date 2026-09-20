-- Compaction + memory policy columns rollback.
ALTER TABLE tenant_settings
  DROP COLUMN IF EXISTS context_compaction_enabled,
  DROP COLUMN IF EXISTS context_compaction_pressure_frac,
  DROP COLUMN IF EXISTS context_recent_turns,
  DROP COLUMN IF EXISTS memory_enabled,
  DROP COLUMN IF EXISTS memory_digest_entries;
