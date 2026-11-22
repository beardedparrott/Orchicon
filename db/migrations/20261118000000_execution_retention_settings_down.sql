-- Reverse of 20261118000000_execution_retention_settings.sql.
--
-- Drops the retention knobs. No DATA is lost by this either way: these columns configure a prune, they do
-- not hold the execution history the prune removes. A deployment that reverts keeps whatever executions
-- the last pass left and simply stops pruning again — which is exactly the pre-migration behaviour
-- (manual `orchicon prune-executions` only), so reverting is safe.
ALTER TABLE tenant_settings
  DROP COLUMN IF EXISTS execution_retention_enabled,
  DROP COLUMN IF EXISTS execution_retention_keep,
  DROP COLUMN IF EXISTS execution_retention_vacuum,
  DROP COLUMN IF EXISTS execution_retention_timeout_seconds,
  DROP COLUMN IF EXISTS outbox_retention_days;
