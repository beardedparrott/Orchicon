-- Remove session TTL columns added in 20260825000000_session_ttl.sql

ALTER TABLE tenant_settings
  DROP COLUMN session_access_token_ttl_seconds,
  DROP COLUMN session_refresh_token_ttl_seconds;
