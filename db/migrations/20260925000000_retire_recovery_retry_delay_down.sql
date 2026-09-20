-- Reverse 20260925000000_retire_recovery_retry_delay.sql: restore the column comments
-- written by 20260721000000_recovery_retry_config.sql.
--
-- Comments are metadata only, so there is no data to restore and nothing to undo — this
-- exists so the paired-down convention holds and a rollback returns the schema's
-- self-description to exactly what the earlier binary shipped.

COMMENT ON COLUMN recovery_executions.retry_delay_seconds
  IS 'Seconds to wait between retries. Set from workflow step config; defaults to 10.';

COMMENT ON COLUMN recovery_executions.max_retries
  IS 'Max retry attempts before escalating to L3 human approval. Set from workflow step config; defaults to 5.';
