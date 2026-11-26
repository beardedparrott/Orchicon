-- Reverse of 20261126000000_antigravity_settings.sql.
--
-- Dropping these columns loses NO credential and no conversation: the auth method is
-- a selection, and the API key itself lives in tenant_secrets (untouched here). A
-- reverted deployment falls back to what the pre-migration code did - a hard-coded
-- oauth-personal with telemetry at the CLI's own default - so reverting is safe and
-- the operator can roll forward again without re-entering anything.
ALTER TABLE tenant_settings
  DROP CONSTRAINT IF EXISTS tenant_settings_antigravity_auth_method_chk;

ALTER TABLE tenant_settings
  DROP COLUMN IF EXISTS antigravity_auth_method,
  DROP COLUMN IF EXISTS antigravity_share_telemetry;
