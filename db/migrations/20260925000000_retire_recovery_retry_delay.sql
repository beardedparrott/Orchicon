-- Correct the column comments on recovery_executions from an intent that was never
-- implemented to what the columns actually are.
--
-- WHY. 20260721000000_recovery_retry_config.sql added both columns "so the workflow
-- step's recovery configuration is plumbed through to the engine", and documented them
-- as such:
--
--   max_retries        'Max retry attempts before escalating to L3 human approval. Set
--                       from workflow step config; defaults to 5.'
--   retry_delay_seconds 'Seconds to wait between retries. Set from workflow step config;
--                       defaults to 10.'
--
-- Neither statement is true now, and reading the schema should not mislead:
--
--   * retry_delay_seconds described behaviour the system does not have. NOTHING ever
--     WAITED on it: the value was written from a constant (engine) and parsed off a
--     task step's config (scheduler), and no code ever read it back. There is no
--     deferral mechanism for execution dispatch at all — retries go out immediately —
--     so the column described a wait that never happened. The knob has been REMOVED from
--     the code (the step-config field and every reader/writer). The COLUMN is retained
--     because up migrations are additive-only (no destructive DDL), so it is now
--     write-nothing, read-nothing: an inert NOT NULL DEFAULT.
--
--   * max_retries IS the retry cap the engine applies, but the escalation check reads
--     the engine's own constant (recovery.defaultMaxRetries), NOT this column. The
--     column is therefore write-only: changing it does not change behaviour.
--
-- This migration is COMMENT statements only — metadata, no DDL, no data. It changes no
-- behaviour; it stops the schema from advertising two controls that do not exist.
--
-- The related stale claim — that both values are overridable by
-- ORCHICON_RECOVERY_MAX_RETRIES / ORCHICON_RECOVERY_RETRY_DELAY_SECONDS — is corrected
-- at source in internal/recovery/engine.go; neither env name was ever read anywhere.

COMMENT ON COLUMN recovery_executions.retry_delay_seconds
  IS 'UNUSED — retained for the additive-only migration convention. Nothing waits on it: execution dispatch has no deferral mechanism, so retries are immediate. The task-step config key of the same name was removed from the code (it was parsed and never read). Read-only historical value.';

COMMENT ON COLUMN recovery_executions.max_retries
  IS 'Write-only. The recovery engine applies a retry cap before escalating to L3, but reads its own constant (recovery.defaultMaxRetries), not this column — so changing this value does not change behaviour. Set from the step config is NOT implemented.';
