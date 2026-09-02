-- MCP server management (Settings → Adapters → MCP): tenant-scoped MCP
-- server entries (stdio | streamable HTTP) with registry provenance and
-- auto-install result recording. The sibling MCP-client task consumes
-- these rows at session time; this task owns storage + management UI.
--
-- env / headers are JSONB maps whose VALUES may be ${SECRET_NAME}
-- references into tenant_secrets (write-only credentials, ADR-0006 D5
-- pattern); plaintext credentials never land here.

CREATE TABLE IF NOT EXISTS "mcp_servers" (
  "id"              TEXT NOT NULL PRIMARY KEY,
  "tenant_id"       TEXT NOT NULL REFERENCES tenants(id),
  "name"            TEXT NOT NULL,
  "transport"       TEXT NOT NULL DEFAULT 'stdio',
  "command"         TEXT NOT NULL DEFAULT '',
  "args"            JSONB NOT NULL DEFAULT '[]'::jsonb,
  "env"             JSONB NOT NULL DEFAULT '{}'::jsonb,
  "url"             TEXT NOT NULL DEFAULT '',
  "headers"         JSONB NOT NULL DEFAULT '{}'::jsonb,
  "enabled"         BOOLEAN NOT NULL DEFAULT true,
  "catalog_slug"    TEXT NOT NULL DEFAULT '',
  "install_status"  TEXT NOT NULL DEFAULT 'unknown',
  "install_result"  JSONB NOT NULL DEFAULT '{}'::jsonb,
  "created_at"      TIMESTAMPTZ NOT NULL DEFAULT now(),
  "updated_at"      TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE ("tenant_id", "name")
);
CREATE INDEX IF NOT EXISTS mcp_servers_tenant_id_idx ON mcp_servers(tenant_id);

ALTER TABLE mcp_servers ENABLE ROW LEVEL SECURITY;
ALTER TABLE mcp_servers FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON mcp_servers;
CREATE POLICY tenant_isolation ON mcp_servers
  FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
