-- ANTIGRAVITY AUTH METHOD + TELEMETRY PREFERENCE (per tenant).
--
-- WHY A COLUMN AND NOT A CONSTANT. agy's ACP server requires an explicit
-- `authenticate {methodId}` before `session/new`. Which method is right is a
-- PER-OPERATOR decision, not a product default:
--
--   * oauth-personal reuses the interactive Google login the CLI already cached.
--     The operator accepts Google's Antigravity terms (which restrict using the
--     service with third-party products) by enabling it, so it is a choice they
--     make - defaulted on, because it is the method most operators already have
--     working, but changeable and revocable.
--   * gemini-api-key is the ONLY method that works inside a runtime container: the
--     personal credential lives in the host OS keyring, which cannot be
--     bind-mounted into a container. This is an API KEY, held as a tenant secret
--     named GEMINI_API_KEY (AES-GCM at rest) and passed to the child as an env var
--     only.
--   * oauth-business / agent-platform are the enterprise paths.
--
-- The CHECK constraint is the database-level half of the validation the Go layer
-- also performs: a bad value must not be storable even by a caller that skips the
-- settings service.
ALTER TABLE tenant_settings
  ADD COLUMN IF NOT EXISTS antigravity_auth_method text NOT NULL DEFAULT 'oauth-personal',
  -- Telemetry is OFF by default. Google's Antigravity terms (section 5) treat usage
  -- telemetry as a separate grant, so sharing it must be an explicit opt-in and a
  -- never-configured tenant must not be sending anything.
  ADD COLUMN IF NOT EXISTS antigravity_share_telemetry boolean NOT NULL DEFAULT false;

DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint WHERE conname = 'tenant_settings_antigravity_auth_method_chk'
  ) THEN
    ALTER TABLE tenant_settings
      ADD CONSTRAINT tenant_settings_antigravity_auth_method_chk
      CHECK (antigravity_auth_method IN ('oauth-personal', 'oauth-business', 'gemini-api-key', 'agent-platform'));
  END IF;
END
$$;
