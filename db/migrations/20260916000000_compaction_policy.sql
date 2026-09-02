-- Compaction + memory policy typed columns on tenant_settings (D4).
--
-- Additive-only (no destructive DDL), mirroring the BudgetLadder migration
-- pattern. These columns are the typed tenant defaults for guarded
-- compaction and the durable agent-memory digest; they serialize into the
-- existing budget-JSON transport (context_compaction / memory keys) that
-- rides mergeBudgets to the adapters, so per-worker overrides layer on top
-- with the same JSON keys.

ALTER TABLE tenant_settings
  ADD COLUMN IF NOT EXISTS context_compaction_enabled BOOLEAN NOT NULL DEFAULT true,
  ADD COLUMN IF NOT EXISTS context_compaction_pressure_frac DOUBLE PRECISION NOT NULL DEFAULT 0.8,
  ADD COLUMN IF NOT EXISTS context_recent_turns INT NOT NULL DEFAULT 6,
  ADD COLUMN IF NOT EXISTS memory_enabled BOOLEAN NOT NULL DEFAULT true,
  ADD COLUMN IF NOT EXISTS memory_digest_entries INT NOT NULL DEFAULT 5;
