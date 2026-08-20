-- Session TTL columns for per-tenant session timeout settings.
-- Defaults match today's compile-time defaults: 15-minute access TTL,
-- 24-hour refresh TTL. Zero-value columns mean "leave unchanged" in the
-- update path; the DB defaults are the actual defaults.

ALTER TABLE tenant_settings
  ADD COLUMN session_access_token_ttl_seconds BIGINT NOT NULL DEFAULT 900,
  ADD COLUMN session_refresh_token_ttl_seconds BIGINT NOT NULL DEFAULT 86400;
