-- Provider settings (ADR-0006 D1): tenant-scoped overrides for built-in
-- provider profiles + tenant-created custom OpenAI-compatible providers.
-- Built-ins get a row only when the operator changes something (enabled=false,
-- base_url_override, hidden models, num_ctx); no row = fully built-in default.
-- is_custom=true rows carry the custom provider definition (display_name,
-- base_url, auth_mode); is_custom=false rows are overrides for built-ins.

CREATE TABLE IF NOT EXISTS "provider_settings" (
  "id"                TEXT NOT NULL PRIMARY KEY,
  "tenant_id"         TEXT NOT NULL REFERENCES tenants(id),
  "provider_id"       TEXT NOT NULL,
  "enabled"           BOOLEAN NOT NULL DEFAULT true,
  "base_url_override" TEXT NOT NULL DEFAULT '',
  "base_url"          TEXT NOT NULL DEFAULT '',
  "auth_mode"         TEXT NOT NULL DEFAULT 'none',
  "num_ctx_default"   BIGINT NOT NULL DEFAULT 0,
  "hidden_models"     JSONB NOT NULL DEFAULT '[]'::jsonb,
  "manual_models"     JSONB NOT NULL DEFAULT '[]'::jsonb,
  "display_name"      TEXT NOT NULL DEFAULT '',
  "is_custom"         BOOLEAN NOT NULL DEFAULT false,
  "created_at"        TIMESTAMPTZ NOT NULL DEFAULT now(),
  "updated_at"        TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE ("tenant_id", "provider_id")
);
CREATE INDEX IF NOT EXISTS provider_settings_tenant_id_idx ON provider_settings(tenant_id);

ALTER TABLE provider_settings ENABLE ROW LEVEL SECURITY;
ALTER TABLE provider_settings FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON provider_settings;
CREATE POLICY tenant_isolation ON provider_settings
  FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
