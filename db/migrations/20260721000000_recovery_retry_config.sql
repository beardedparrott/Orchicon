-- Add max_retries and retry_delay_seconds to recovery_executions so the
-- workflow step's recovery configuration is plumbed through to the engine.
ALTER TABLE recovery_executions
  ADD COLUMN max_retries int NOT NULL DEFAULT 5,
  ADD COLUMN retry_delay_seconds int NOT NULL DEFAULT 10;

COMMENT ON COLUMN recovery_executions.max_retries
  IS 'Max retry attempts before escalating to L3 human approval. Set from workflow step config; defaults to 5.';
COMMENT ON COLUMN recovery_executions.retry_delay_seconds
  IS 'Seconds to wait between retries. Set from workflow step config; defaults to 10.';
